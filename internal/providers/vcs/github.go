package vcs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codenite/worker/internal/agent"
	"codenite/worker/internal/util"
)

const githubAPIBase = "https://api.github.com"

type GitHubProvider struct {
	token    string
	workRoot string
	client   *http.Client
}

func NewGitHubProvider(token, workRoot string) *GitHubProvider {
	return &GitHubProvider{
		token:    token,
		workRoot: workRoot,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (g *GitHubProvider) PrepareRepo(ctx context.Context, repo agent.RepoTarget) (string, error) {
	repoPath := filepath.Join(g.workRoot, strings.ReplaceAll(repo.FullName, "/", "__"))
	if err := os.MkdirAll(g.workRoot, 0o755); err != nil {
		return "", fmt.Errorf("create work root: %w", err)
	}

	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		cloneURL := g.cloneURL(repo.FullName)
		res, runErr := util.RunCmd(ctx, "", nil, "git", "clone", cloneURL, repoPath)
		if runErr != nil {
			return "", runErr
		}
		if res.ExitCode != 0 {
			return "", fmt.Errorf("git clone failed: %s", strings.TrimSpace(res.Stderr))
		}
	}

	if err := g.checkoutBase(ctx, repoPath, repo.BaseBranch); err != nil {
		return "", err
	}
	return repoPath, nil
}

func (g *GitHubProvider) CreateBranch(ctx context.Context, repoPath, branchName, baseBranch string) error {
	res, err := util.RunCmd(ctx, repoPath, nil, "git", "checkout", "-B", branchName, baseBranch)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("git checkout branch failed: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (g *GitHubProvider) CommitAll(ctx context.Context, repoPath, message string) (bool, error) {
	addRes, err := util.RunCmd(ctx, repoPath, nil, "git", "add", "-A")
	if err != nil {
		return false, err
	}
	if addRes.ExitCode != 0 {
		return false, fmt.Errorf("git add failed: %s", strings.TrimSpace(addRes.Stderr))
	}

	diffRes, err := util.RunCmd(ctx, repoPath, nil, "git", "diff", "--cached", "--quiet")
	if err != nil {
		return false, err
	}
	if diffRes.ExitCode == 0 {
		return false, nil
	}
	if diffRes.ExitCode != 1 {
		return false, fmt.Errorf("git diff failed: %s", strings.TrimSpace(diffRes.Stderr))
	}

	commitRes, err := util.RunCmd(ctx, repoPath, nil, "git", "commit", "-m", message)
	if err != nil {
		return false, err
	}
	if commitRes.ExitCode != 0 {
		return false, fmt.Errorf("git commit failed: %s", strings.TrimSpace(commitRes.Stderr))
	}

	return true, nil
}

func (g *GitHubProvider) Push(ctx context.Context, repoPath, branchName string) error {
	res, err := util.RunCmd(ctx, repoPath, nil, "git", "push", "--set-upstream", "origin", branchName)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("git push failed: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (g *GitHubProvider) OpenPullRequest(ctx context.Context, repoFullName, base, head, title, body string, draft bool) (string, error) {
	payload := map[string]any{
		"title": title,
		"head":  head,
		"base":  base,
		"body":  body,
		"draft": draft,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	u := fmt.Sprintf("%s/repos/%s/pulls", githubAPIBase, repoFullName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		resBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("github create pr failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(resBody)))
	}

	var parsed struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if parsed.HTMLURL == "" {
		return "", fmt.Errorf("github create pr returned empty html_url")
	}
	return parsed.HTMLURL, nil
}

func (g *GitHubProvider) checkoutBase(ctx context.Context, repoPath, baseBranch string) error {
	fetchRes, err := util.RunCmd(ctx, repoPath, nil, "git", "fetch", "origin", baseBranch)
	if err != nil {
		return err
	}
	if fetchRes.ExitCode != 0 {
		return fmt.Errorf("git fetch failed: %s", strings.TrimSpace(fetchRes.Stderr))
	}

	checkoutRes, err := util.RunCmd(ctx, repoPath, nil, "git", "checkout", baseBranch)
	if err != nil {
		return err
	}
	if checkoutRes.ExitCode != 0 {
		checkoutRes, err = util.RunCmd(ctx, repoPath, nil, "git", "checkout", "-B", baseBranch, "origin/"+baseBranch)
		if err != nil {
			return err
		}
		if checkoutRes.ExitCode != 0 {
			return fmt.Errorf("git checkout base failed: %s", strings.TrimSpace(checkoutRes.Stderr))
		}
	}

	pullRes, err := util.RunCmd(ctx, repoPath, nil, "git", "pull", "--ff-only", "origin", baseBranch)
	if err != nil {
		return err
	}
	if pullRes.ExitCode != 0 {
		return fmt.Errorf("git pull failed: %s", strings.TrimSpace(pullRes.Stderr))
	}

	return nil
}

func (g *GitHubProvider) cloneURL(repoFullName string) string {
	if g.token == "" {
		return fmt.Sprintf("https://github.com/%s.git", repoFullName)
	}
	return fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", g.token, repoFullName)
}
