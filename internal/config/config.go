package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type Config struct {
	Worker       WorkerConfig                `json:"worker"`
	TaskSource   TaskSourceConfig            `json:"task_source"`
	AI           AIConfig                    `json:"ai"`
	VCS          VCSConfig                   `json:"vcs"`
	Repositories map[string]RepositoryConfig `json:"repositories"`
}

type WorkerConfig struct {
	PollIntervalSeconds int    `json:"poll_interval_seconds"`
	WorkRoot            string `json:"work_root"`
	DryRun              bool   `json:"dry_run"`
	CloseTaskOnPR       bool   `json:"close_task_on_pr"`
	CommentOnTask       bool   `json:"comment_on_task"`
}

type TaskSourceConfig struct {
	Provider string        `json:"provider"`
	Todoist  TodoistConfig `json:"todoist"`
}

type TodoistConfig struct {
	Token  string `json:"token"`
	Label  string `json:"label"`
	Filter string `json:"filter"`
}

type AIConfig struct {
	Provider string            `json:"provider"`
	Model    string            `json:"model"`
	Env      map[string]string `json:"env"`
}

type VCSConfig struct {
	Provider string       `json:"provider"`
	GitHub   GitHubConfig `json:"github"`
}

type GitHubConfig struct {
	Token string `json:"token"`
	Draft bool   `json:"draft"`
}

type RepositoryConfig struct {
	Repo       string `json:"repo"`
	BaseBranch string `json:"base_branch"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	// Allow config values like "${TODOIST_TOKEN}" to be resolved from process env.
	expanded := os.ExpandEnv(string(data))
	cfg := Config{}
	if err := json.Unmarshal([]byte(expanded), &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if c.Worker.PollIntervalSeconds <= 0 {
		return errors.New("worker.poll_interval_seconds must be > 0")
	}
	if c.Worker.WorkRoot == "" {
		return errors.New("worker.work_root is required")
	}
	if c.TaskSource.Provider != "todoist" {
		return errors.New("task_source.provider must be 'todoist'")
	}
	if c.TaskSource.Todoist.Token == "" {
		return errors.New("task_source.todoist.token is required")
	}
	if c.TaskSource.Todoist.Label == "" && c.TaskSource.Todoist.Filter == "" {
		c.TaskSource.Todoist.Label = "ia:do"
	}
	if c.AI.Provider != "codex" {
		return errors.New("ai.provider must be 'codex'")
	}
	if c.AI.Model == "" {
		c.AI.Model = "gpt-5.2-codex"
	}
	if c.VCS.Provider != "github" {
		return errors.New("vcs.provider must be 'github'")
	}
	if c.VCS.GitHub.Token == "" {
		return errors.New("vcs.github.token is required")
	}
	if len(c.Repositories) == 0 {
		return errors.New("repositories map is required")
	}
	for projectID, repo := range c.Repositories {
		if projectID == "" || repo.Repo == "" {
			return errors.New("repositories must map todoist project IDs to repo names")
		}
		if repo.BaseBranch == "" {
			repo.BaseBranch = "main"
			c.Repositories[projectID] = repo
		}
	}
	return nil
}
