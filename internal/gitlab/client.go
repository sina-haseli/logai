package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

const (
	httpTimeout  = 30 * time.Second
	bodyExcerpts = 512
)

// Client is a reusable self-hosted GitLab REST API v4 client.
type Client struct {
	BaseURL    string // e.g. https://gitlab.example.com
	Token      string
	ProjectID  string
	HTTPClient *http.Client
	logger     *slog.Logger
}

// New constructs a GitLab client with a 30s HTTP timeout.
func New(baseURL, token, projectID string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		BaseURL:    baseURL,
		Token:      token,
		ProjectID:  projectID,
		HTTPClient: &http.Client{Timeout: httpTimeout},
		logger:     logger,
	}
}

// projectPath returns the URL-encoded /projects/:id base for the API.
func (c *Client) projectPath() string {
	return fmt.Sprintf("%s/api/v4/projects/%s", c.BaseURL, url.PathEscape(c.ProjectID))
}

// do performs an authenticated request, logs it, and returns the response body.
func (c *Client) do(ctx context.Context, method, endpoint string, body any) ([]byte, int, error) {
	start := time.Now()

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("gitlab: marshal body: %w", err)
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("gitlab: new request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("gitlab: do request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("gitlab: read body: %w", err)
	}

	c.logger.Info("gitlab api call",
		"method", method,
		"endpoint", endpoint,
		"status", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds(),
	)

	return raw, resp.StatusCode, nil
}

func excerpt(b []byte) string {
	if len(b) > bodyExcerpts {
		return string(b[:bodyExcerpts])
	}
	return string(b)
}

// GetFileContent fetches the raw content of a file at the given ref.
func (c *Client) GetFileContent(ctx context.Context, filePath, ref string) (string, error) {
	endpoint := fmt.Sprintf("%s/repository/files/%s/raw?ref=%s",
		c.projectPath(), url.PathEscape(filePath), url.QueryEscape(ref))

	raw, status, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("gitlab: get file content: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("gitlab: get file content: status %d: %s", status, excerpt(raw))
	}
	return string(raw), nil
}

// CreateBranch creates a new branch from ref.
func (c *Client) CreateBranch(ctx context.Context, branchName, ref string) error {
	endpoint := fmt.Sprintf("%s/repository/branches?branch=%s&ref=%s",
		c.projectPath(), url.QueryEscape(branchName), url.QueryEscape(ref))

	raw, status, err := c.do(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("gitlab: create branch: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("gitlab: create branch: status %d: %s", status, excerpt(raw))
	}
	return nil
}

// CommitFile updates an existing file on a branch with a single commit.
func (c *Client) CommitFile(ctx context.Context, branchName, filePath, content, commitMessage string) error {
	endpoint := fmt.Sprintf("%s/repository/files/%s",
		c.projectPath(), url.PathEscape(filePath))

	body := map[string]string{
		"branch":         branchName,
		"content":        content,
		"commit_message": commitMessage,
	}

	raw, status, err := c.do(ctx, http.MethodPut, endpoint, body)
	if err != nil {
		return fmt.Errorf("gitlab: commit file: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("gitlab: commit file: status %d: %s", status, excerpt(raw))
	}
	return nil
}

type createMRResponse struct {
	IID    int    `json:"iid"`
	WebURL string `json:"web_url"`
}

// CreateMR opens a merge request from branchName into targetBranch.
// If isDraft is true the title is prefixed with "Draft:". assigneeID is applied
// when > 0.
func (c *Client) CreateMR(ctx context.Context, branchName, targetBranch, title, description string, isDraft bool, assigneeID int) (string, int, error) {
	if isDraft {
		title = "Draft: " + title
	}

	endpoint := fmt.Sprintf("%s/merge_requests", c.projectPath())

	body := map[string]any{
		"source_branch":        branchName,
		"target_branch":        targetBranch,
		"title":                title,
		"description":          description,
		"remove_source_branch": true,
	}
	if assigneeID > 0 {
		body["assignee_id"] = assigneeID
	}

	raw, status, err := c.do(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return "", 0, fmt.Errorf("gitlab: create mr: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", 0, fmt.Errorf("gitlab: create mr: status %d: %s", status, excerpt(raw))
	}

	var parsed createMRResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", 0, fmt.Errorf("gitlab: create mr: unmarshal response: %w", err)
	}
	return parsed.WebURL, parsed.IID, nil
}
