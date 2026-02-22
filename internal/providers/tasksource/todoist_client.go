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

const todoistBaseURL = "https://api.todoist.com/rest/v2"

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

func (c *todoistHTTPClient) FetchByLabel(ctx context.Context, label string) ([]todoistTask, error) {
	u := todoistBaseURL + "/tasks?label=" + url.QueryEscape(label)
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
		return nil, fmt.Errorf("todoist fetch failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var raw []todoistTask
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	return raw, nil
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

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("todoist close failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
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

	if resp.StatusCode != http.StatusOK {
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

	if resp.StatusCode != http.StatusOK {
		resBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("todoist update labels failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(resBody)))
	}
	return nil
}
