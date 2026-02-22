package agent

import "context"

type Task struct {
	ID          string
	Title       string
	Description string
	Labels      []string
	ProjectID   string
}

type RepoTarget struct {
	FullName   string
	BaseBranch string
}

type TaskSource interface {
	Fetch(context.Context) ([]Task, error)
	Close(context.Context, string) error
	Comment(context.Context, string, string) error
	UpdateLabels(context.Context, string, []string) error
}

type AIProvider interface {
	Develop(ctx context.Context, repoPath, repoFullName string, task Task) (AIResult, error)
}

type AIResult struct {
	Stdout      string
	Stderr      string
	ExitCode    int
	Summary     string
	ChangedFiles []string
}

type VCSProvider interface {
	PrepareRepo(ctx context.Context, repo RepoTarget) (string, error)
	CreateBranch(ctx context.Context, repoPath, branchName, baseBranch string) error
	CommitAll(ctx context.Context, repoPath, message string) (bool, error)
	Push(ctx context.Context, repoPath, branchName string) error
	OpenPullRequest(ctx context.Context, repoFullName, base, head, title, body string, draft bool) (string, error)
}
