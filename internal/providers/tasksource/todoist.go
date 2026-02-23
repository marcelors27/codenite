package tasksource

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"codenite/worker/internal/agent"
)

const (
	codingLabel   = "ai:coding"
	prOpenedLabel = "ai:pr-done"
)

type TodoistSource struct {
	label  string
	filter string
	client todoistClient
}

type todoistClient interface {
	FetchTasks(context.Context, string, string) ([]todoistTask, error)
	FetchTaskComments(context.Context, string) ([]todoistComment, error)
	CloseTask(context.Context, string) error
	CommentTask(context.Context, string, string) error
	UpdateTaskLabels(context.Context, string, []string) error
}

func NewTodoistSource(token, label, filter string) *TodoistSource {
	return NewTodoistSourceWithClient(label, filter, newTodoistHTTPClient(token))
}

func NewTodoistSourceWithClient(label, filter string, client todoistClient) *TodoistSource {
	if strings.TrimSpace(label) == "" && strings.TrimSpace(filter) == "" {
		label = "ai:do"
	}
	return &TodoistSource{
		label: label,
		filter: filter,
		client: client,
	}
}

type todoistTask struct {
	ID          any      `json:"id"`
	Content     string   `json:"content"`
	Description string   `json:"description"`
	Labels      []string `json:"labels"`
	ProjectID   any      `json:"project_id"`
}

type todoistComment struct {
	Content string `json:"content"`
}

func (t *TodoistSource) Fetch(ctx context.Context) ([]agent.Task, error) {
	raw, err := t.client.FetchTasks(ctx, t.label, t.filter)
	if err != nil {
		return nil, err
	}

	out := make([]agent.Task, 0, len(raw))
	for _, task := range raw {
		if hasLabel(task.Labels, codingLabel) || hasLabel(task.Labels, prOpenedLabel) {
			continue
		}
		out = append(out, agent.Task{
			ID:          anyToString(task.ID),
			Title:       strings.TrimSpace(task.Content),
			Description: strings.TrimSpace(task.Description),
			Labels:      task.Labels,
			ProjectID:   anyToString(task.ProjectID),
		})
	}
	return out, nil
}

func (t *TodoistSource) Close(ctx context.Context, taskID string) error {
	return t.client.CloseTask(ctx, taskID)
}

func (t *TodoistSource) FetchByLabel(ctx context.Context, label string) ([]agent.Task, error) {
	raw, err := t.client.FetchTasks(ctx, label, "")
	if err != nil {
		return nil, err
	}

	out := make([]agent.Task, 0, len(raw))
	for _, task := range raw {
		out = append(out, agent.Task{
			ID:          anyToString(task.ID),
			Title:       strings.TrimSpace(task.Content),
			Description: strings.TrimSpace(task.Description),
			Labels:      task.Labels,
			ProjectID:   anyToString(task.ProjectID),
		})
	}
	return out, nil
}

func (t *TodoistSource) Comment(ctx context.Context, taskID, text string) error {
	return t.client.CommentTask(ctx, taskID, text)
}

func (t *TodoistSource) FindPRURL(ctx context.Context, taskID string) (string, error) {
	comments, err := t.client.FetchTaskComments(ctx, taskID)
	if err != nil {
		return "", err
	}

	re := regexp.MustCompile(`https://github\.com/[^\s]+/pull/[0-9]+`)
	for i := len(comments) - 1; i >= 0; i-- {
		match := re.FindString(strings.TrimSpace(comments[i].Content))
		if match != "" {
			return match, nil
		}
	}
	return "", nil
}

func (t *TodoistSource) UpdateLabels(ctx context.Context, taskID string, labels []string) error {
	return t.client.UpdateTaskLabels(ctx, taskID, labels)
}

func anyToString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case json.Number:
		return x.String()
	default:
		return fmt.Sprintf("%v", x)
	}
}

func hasLabel(labels []string, target string) bool {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}
