package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codenite/worker/internal/agent"
	"codenite/worker/internal/util"
)

const (
	openAIResponsesURL = "https://api.openai.com/v1/responses"
	maxRepoFiles       = 800
	maxReadFiles       = 24
	maxFileBytes       = 120_000
	maxContextBytes    = 320_000
	openAITimeout      = 300 * time.Second
	maxOpenAIRetries   = 4
	maxOpenAIBodyBytes = 20 * 1024 * 1024
)

type CodexProvider struct {
	model  string
	env    map[string]string
	client *http.Client
}

type repoFile struct {
	Path string
	Size int64
}

type fileSelection struct {
	ReadFiles []string `json:"read_files"`
}

type fileChange struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type changePlan struct {
	Summary string       `json:"summary"`
	Changes []fileChange `json:"changes"`
}

type openAIRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type openAIResponse struct {
	Output []struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

func NewCodexProvider(model string, env map[string]string) *CodexProvider {
	if strings.TrimSpace(model) == "" {
		model = "gpt-5.2-codex"
	}
	return &CodexProvider{
		model: model,
		env:   env,
		client: &http.Client{
			Timeout: openAITimeout,
		},
	}
}

func (p *CodexProvider) Develop(ctx context.Context, repoPath, repoFullName string, task agent.Task) (agent.AIResult, error) {
	apiKey := p.openAIKey()
	if apiKey == "" {
		return agent.AIResult{ExitCode: 1}, fmt.Errorf("openai api key not configured")
	}

	files, err := listRepoFiles(ctx, repoPath)
	if err != nil {
		return agent.AIResult{ExitCode: 1}, fmt.Errorf("list repo files: %w", err)
	}

	selectPrompt := buildSelectionPrompt(task, repoFullName, files)
	selectRaw, err := p.callResponses(ctx, apiKey, selectPrompt)
	if err != nil {
		return agent.AIResult{ExitCode: 1}, err
	}
	selection, err := parseSelection(selectRaw)
	if err != nil {
		return agent.AIResult{Stdout: selectRaw, ExitCode: 1}, fmt.Errorf("parse selected files: %w", err)
	}

	selected := normalizeRequestedFiles(selection.ReadFiles, files)
	fileContents, err := readSelectedFiles(repoPath, selected)
	if err != nil {
		return agent.AIResult{Stdout: selectRaw, ExitCode: 1}, fmt.Errorf("read selected files: %w", err)
	}

	editPrompt := buildEditPrompt(task, repoFullName, fileContents)
	editRaw, err := p.callResponses(ctx, apiKey, editPrompt)
	if err != nil {
		return agent.AIResult{Stdout: selectRaw, ExitCode: 1}, err
	}
	plan, err := parseChangePlan(editRaw)
	if err != nil {
		stdout := selectRaw + "\n\n" + editRaw
		return agent.AIResult{Stdout: stdout, ExitCode: 1}, fmt.Errorf("parse change plan: %w", err)
	}
	if len(plan.Changes) == 0 {
		stdout := selectRaw + "\n\n" + editRaw
		return agent.AIResult{Stdout: stdout, ExitCode: 1}, fmt.Errorf("ai returned no file changes")
	}

	if err := applyChanges(repoPath, plan.Changes); err != nil {
		stdout := selectRaw + "\n\n" + editRaw
		return agent.AIResult{Stdout: stdout, ExitCode: 1}, fmt.Errorf("apply changes: %w", err)
	}

	stdout := strings.TrimSpace(selectRaw + "\n\n" + editRaw + "\n\nsummary: " + plan.Summary)
	changedFiles := make([]string, 0, len(plan.Changes))
	for _, ch := range plan.Changes {
		if rel, err := sanitizeRelativePath(ch.Path); err == nil {
			changedFiles = append(changedFiles, rel)
		}
	}
	sort.Strings(changedFiles)
	return agent.AIResult{
		Stdout:       stdout,
		Stderr:       "",
		ExitCode:     0,
		Summary:      strings.TrimSpace(plan.Summary),
		ChangedFiles: changedFiles,
	}, nil
}

func (p *CodexProvider) openAIKey() string {
	if p.env != nil {
		if raw, ok := p.env["OPENAI_API_KEY"]; ok {
			value := strings.TrimSpace(os.ExpandEnv(raw))
			if value != "" {
				return value
			}
		}
	}
	return strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
}

func (p *CodexProvider) callResponses(ctx context.Context, apiKey, prompt string) (string, error) {
	reqBody, err := json.Marshal(openAIRequest{
		Model: p.model,
		Input: prompt,
	})
	if err != nil {
		return "", err
	}

	var lastErr error
	for attempt := 1; attempt <= maxOpenAIRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIResponsesURL, bytes.NewReader(reqBody))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := p.client.Do(req)
		if err != nil {
			lastErr = err
			if !isRetryableOpenAIError(err) || attempt == maxOpenAIRetries {
				return "", err
			}
			sleepWithContext(ctx, retryDelay(attempt))
			continue
		}

		rawBody, err := readBodyCapped(resp.Body, maxOpenAIBodyBytes)
		if err != nil {
			return "", err
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("openai responses failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(rawBody)))
			if !isRetryableOpenAIStatus(resp.StatusCode) || attempt == maxOpenAIRetries {
				return "", lastErr
			}
			sleepWithContext(ctx, retryDelay(attempt))
			continue
		}

		var parsed openAIResponse
		if err := json.Unmarshal(rawBody, &parsed); err != nil {
			return "", fmt.Errorf("parse openai response json: %w; body_prefix=%q", err, prefixForLog(string(rawBody), 500))
		}

		var out strings.Builder
		for _, item := range parsed.Output {
			for _, content := range item.Content {
				if strings.TrimSpace(content.Text) == "" {
					continue
				}
				if out.Len() > 0 {
					out.WriteString("\n")
				}
				out.WriteString(content.Text)
			}
		}
		if strings.TrimSpace(out.String()) == "" {
			return "", fmt.Errorf("empty model output")
		}
		return out.String(), nil
	}

	return "", lastErr
}

func isRetryableOpenAIStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

func isRetryableOpenAIError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func retryDelay(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 2 * time.Second
	case 2:
		return 5 * time.Second
	case 3:
		return 10 * time.Second
	default:
		return 15 * time.Second
	}
}

func sleepWithContext(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func readBodyCapped(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return io.ReadAll(r)
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("openai response body too large (> %d bytes)", maxBytes)
	}
	return data, nil
}

func prefixForLog(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}

func listRepoFiles(ctx context.Context, repoPath string) ([]repoFile, error) {
	res, err := util.RunCmd(ctx, repoPath, nil, "git", "ls-files")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("git ls-files failed: %s", strings.TrimSpace(res.Stderr))
	}

	lines := strings.Split(res.Stdout, "\n")
	files := make([]repoFile, 0, len(lines))
	for _, line := range lines {
		path := strings.TrimSpace(line)
		if path == "" {
			continue
		}
		full := filepath.Join(repoPath, path)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Size() > maxFileBytes {
			continue
		}
		files = append(files, repoFile{Path: path, Size: info.Size()})
		if len(files) >= maxRepoFiles {
			break
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func buildSelectionPrompt(task agent.Task, repoFullName string, files []repoFile) string {
	var b strings.Builder
	b.WriteString("You are coding in a Git repository.\n")
	b.WriteString("Select which files must be read before implementing the task.\n")
	b.WriteString("Return ONLY JSON with shape: {\"read_files\":[\"path\"]}\n")
	b.WriteString(fmt.Sprintf("Max %d files.\n\n", maxReadFiles))
	b.WriteString("Task:\n")
	b.WriteString("ID: " + task.ID + "\n")
	b.WriteString("Title: " + task.Title + "\n")
	if strings.TrimSpace(task.Description) != "" {
		b.WriteString("Description: " + task.Description + "\n")
	}
	b.WriteString("Repo: " + repoFullName + "\n\n")
	b.WriteString("Available files (path | bytes):\n")
	for _, f := range files {
		b.WriteString(fmt.Sprintf("- %s | %d\n", f.Path, f.Size))
	}
	return b.String()
}

func parseSelection(raw string) (fileSelection, error) {
	jsonText, err := extractJSONObject(raw)
	if err != nil {
		return fileSelection{}, err
	}
	var out fileSelection
	if err := json.Unmarshal([]byte(jsonText), &out); err != nil {
		return fileSelection{}, err
	}
	return out, nil
}

func normalizeRequestedFiles(requested []string, allowed []repoFile) []string {
	if len(requested) == 0 {
		return nil
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, f := range allowed {
		allowedSet[f.Path] = struct{}{}
	}

	seen := map[string]struct{}{}
	out := make([]string, 0, maxReadFiles)
	for _, path := range requested {
		path = filepath.ToSlash(strings.TrimSpace(path))
		if path == "" {
			continue
		}
		if _, ok := allowedSet[path]; !ok {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
		if len(out) >= maxReadFiles {
			break
		}
	}
	return out
}

func readSelectedFiles(repoPath string, paths []string) (map[string]string, error) {
	out := make(map[string]string, len(paths))
	total := 0
	for _, rel := range paths {
		full := filepath.Join(repoPath, rel)
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", rel, err)
		}
		if len(data) > maxFileBytes {
			data = data[:maxFileBytes]
		}
		if total+len(data) > maxContextBytes {
			break
		}
		total += len(data)
		out[rel] = string(data)
	}
	return out, nil
}

func buildEditPrompt(task agent.Task, repoFullName string, files map[string]string) string {
	var b strings.Builder
	b.WriteString("Implement the task by editing repository files.\n")
	b.WriteString("Return ONLY JSON with shape:\n")
	b.WriteString("{\"summary\":\"short text\",\"changes\":[{\"path\":\"relative/path\",\"content\":\"full file content\"}]}\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Use relative paths.\n")
	b.WriteString("- content must be the complete final content for each file.\n")
	b.WriteString("- Keep changes minimal and safe.\n")
	b.WriteString("- Prefer editing provided files. Creating new files is allowed when necessary.\n\n")
	b.WriteString("Task:\n")
	b.WriteString("ID: " + task.ID + "\n")
	b.WriteString("Title: " + task.Title + "\n")
	if strings.TrimSpace(task.Description) != "" {
		b.WriteString("Description: " + task.Description + "\n")
	}
	b.WriteString("Repo: " + repoFullName + "\n\n")
	b.WriteString("File contents:\n")

	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, path := range keys {
		b.WriteString("\n--- FILE: " + path + " ---\n")
		b.WriteString(files[path])
		b.WriteString("\n--- END FILE ---\n")
	}
	return b.String()
}

func parseChangePlan(raw string) (changePlan, error) {
	jsonText, err := extractJSONObject(raw)
	if err != nil {
		return changePlan{}, err
	}
	var out changePlan
	if err := json.Unmarshal([]byte(jsonText), &out); err != nil {
		return changePlan{}, fmt.Errorf("%w; json_prefix=%q", err, prefixForLog(jsonText, 500))
	}
	return out, nil
}

func extractJSONObject(text string) (string, error) {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return "", fmt.Errorf("json object not found")
	}

	inString := false
	escape := false
	depth := 0
	for i := start; i < len(text); i++ {
		ch := text[i]
		if inString {
			if escape {
				escape = false
				continue
			}
			if ch == '\\' {
				escape = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			continue
		}
		if ch == '{' {
			depth++
			continue
		}
		if ch == '}' {
			depth--
			if depth == 0 {
				return text[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unterminated json object")
}

func applyChanges(repoPath string, changes []fileChange) error {
	for _, ch := range changes {
		rel, err := sanitizeRelativePath(ch.Path)
		if err != nil {
			return err
		}
		full := filepath.Join(repoPath, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(ch.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func sanitizeRelativePath(path string) (string, error) {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" {
		return "", fmt.Errorf("empty change path")
	}
	if strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("absolute path not allowed: %s", path)
	}
	clean := filepath.Clean(path)
	if clean == "." || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("path escapes repo: %s", path)
	}
	return clean, nil
}
