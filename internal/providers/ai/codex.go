package ai

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"codenite/worker/internal/agent"
	"codenite/worker/internal/util"
)

type CodexProvider struct {
	command string
	env     map[string]string
}

func NewCodexProvider(command string, env map[string]string) *CodexProvider {
	return &CodexProvider{command: command, env: env}
}

func (p *CodexProvider) Develop(ctx context.Context, repoPath, repoFullName string, task agent.Task) (agent.AIResult, error) {
	prompt := buildPrompt(task)
	env := []string{
		"REPO_PATH=" + repoPath,
		"REPO=" + repoFullName,
		"TASK_ID=" + task.ID,
		"TASK_TITLE=" + task.Title,
		"TASK_DESCRIPTION=" + task.Description,
		"TASK_PROMPT=" + prompt,
	}
	commandEnv := p.commandEnv()
	if !hasEnvKey(commandEnv, "OPENAI_API_KEY") {
		if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" {
			commandEnv = append(commandEnv, "OPENAI_API_KEY="+key)
		}
	}
	env = append(env, commandEnv...)

	res, err := util.RunCmd(ctx, repoPath, env, "sh", "-lc", p.command)
	if err != nil {
		return agent.AIResult{ExitCode: -1}, err
	}
	out := agent.AIResult{
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
	}
	if res.ExitCode != 0 {
		return out, fmt.Errorf("codex command failed: %s", strings.TrimSpace(res.Stderr))
	}
	return out, nil
}

func (p *CodexProvider) commandEnv() []string {
	if len(p.env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(p.env))
	for k := range p.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		// Supports values like "${OPENAI_API_KEY}" from deployment environment.
		value := os.ExpandEnv(strings.TrimSpace(p.env[key]))
		if strings.TrimSpace(value) == "" {
			continue
		}
		out = append(out, key+"="+value)
	}
	return out
}

func hasEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}

func buildPrompt(task agent.Task) string {
	text := "Implemente a task no código atual com testes quando fizer sentido.\\n"
	text += "Task ID: " + task.ID + "\\n"
	text += "Título: " + task.Title + "\\n"
	if strings.TrimSpace(task.Description) != "" {
		text += "Descrição: " + task.Description + "\\n"
	}
	text += "Faça mudanças pequenas, seguras e prontas para PR."
	return text
}
