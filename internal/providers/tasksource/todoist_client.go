package tasksource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const todoistBaseURL = "https://api.todoist.com/api/v1"

type todoistTasksPage struct {
	Results    []todoistTask `json:"results"`
	NextCursor string        `json:"next_cursor"`
}

type todoistCommentsPage struct {
	Results []todoistComment `json:"results"`
}

type todoistHTTPClient struct {
	token  string
	client *http.Client
}

func newTodoistHTTPClient(token string) *todoistHTTPClient {
	return &todoistHTTPClient{
		token: token,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *todoistHTTPClient) FetchTasks(ctx context.Context, label, filter string) ([]todoistTask, error) {
	basePath := "/tasks"
	params := url.Values{}
	params.Set("limit", "200")

	if strings.TrimSpace(filter) != "" {
		basePath = "/tasks/filter"
		params.Set("query", filter)
	} else {
		params.Set("label", label)
	}

	out := make([]todoistTask, 0)
	cursor := ""
	for {
		if cursor == "" {
			params.Del("cursor")
		} else {
			params.Set("cursor", cursor)
		}

		u := todoistBaseURL + basePath + "?" + params.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)

		resp, err := c.client.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			return nil, fmt.Errorf("todoist fetch failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var page todoistTasksPage
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()

		out = append(out, page.Results...)
		if strings.TrimSpace(page.NextCursor) == "" {
			break
		}
		cursor = page.NextCursor
	}

	return out, nil
}

func (c *todoistHTTPClient) CloseTask(ctx context.Context, taskID string) error {
	u := fmt.Sprintf("%s/tasks/%s/close", todoistBaseURL, url.PathEscape(taskID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("todoist close failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *todoistHTTPClient) FetchTaskComments(ctx context.Context, taskID string) ([]todoistComment, error) {
	params := url.Values{}
	params.Set("task_id", taskID)
	params.Set("limit", "200")

	u := todoistBaseURL + "/comments?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("todoist fetch comments failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	resBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var comments []todoistComment
	if err := json.Unmarshal(resBody, &comments); err == nil {
		return comments, nil
	}

	var page todoistCommentsPage
	if err := json.Unmarshal(resBody, &page); err == nil {
		return page.Results, nil
	}

	return nil, fmt.Errorf("todoist fetch comments: unsupported response format")
}

func (c *todoistHTTPClient) CommentTask(ctx context.Context, taskID, text string) error {
	payload := map[string]string{"task_id": taskID, "content": text}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, todoistBaseURL+"/comments", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		resBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("todoist comment failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(resBody)))
	}
	return nil
}

func (c *todoistHTTPClient) UpdateTaskLabels(ctx context.Context, taskID string, labels []string) error {
	payload := map[string][]string{"labels": labels}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	u := fmt.Sprintf("%s/tasks/%s", todoistBaseURL, url.PathEscape(taskID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		resBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("todoist update labels failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(resBody)))
	}
	return nil
}
