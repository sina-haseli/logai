package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
	model          = "claude-sonnet-4-6"
	maxTokens      = 1024
	httpTimeout    = 90 * time.Second
	bodyExcerpts   = 512
)

// Client is a reusable Anthropic Messages API client. BaseURL allows pointing
// at an Anthropic-compatible gateway (e.g. a proxy) instead of the public API.
type Client struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
	logger     *slog.Logger
}

// New constructs a Client with a 90s HTTP timeout. An empty baseURL falls back
// to the public Anthropic API.
func New(apiKey, baseURL string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		APIKey:     apiKey,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: httpTimeout},
		logger:     logger,
	}
}

type requestBody struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	System    string           `json:"system,omitempty"`
	Messages  []requestMessage `json:"messages"`
}

type requestMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseBody struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Complete sends a single-turn message and returns the concatenated text content.
// stage is used only for structured logging.
func (c *Client) Complete(ctx context.Context, stage, systemPrompt, userPrompt string) (string, error) {
	start := time.Now()

	reqBody := requestBody{
		Model:     model,
		MaxTokens: maxTokens,
		System:    systemPrompt,
		Messages:  []requestMessage{{Role: "user", Content: userPrompt}},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("anthropic: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("anthropic: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", apiVersion)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic: do request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("anthropic: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		excerpt := string(raw)
		if len(excerpt) > bodyExcerpts {
			excerpt = excerpt[:bodyExcerpts]
		}
		return "", fmt.Errorf("anthropic: unexpected status %d: %s", resp.StatusCode, excerpt)
	}

	var parsed responseBody
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("anthropic: unmarshal response: %w", err)
	}

	var sb strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	text := sb.String()

	c.logger.Info("anthropic completion",
		"stage", stage,
		"model", model,
		"duration_ms", time.Since(start).Milliseconds(),
		"input_tokens", parsed.Usage.InputTokens,
		"output_tokens", parsed.Usage.OutputTokens,
	)

	if text == "" {
		return "", fmt.Errorf("anthropic: empty text content in response")
	}
	return text, nil
}

// ParseJSON strips optional markdown code fences and unmarshals the model's
// response into v. Use this for all structured-output stages.
func ParseJSON(raw string, v any) error {
	cleaned := StripFences(raw)
	if err := json.Unmarshal([]byte(cleaned), v); err != nil {
		excerpt := cleaned
		if len(excerpt) > bodyExcerpts {
			excerpt = excerpt[:bodyExcerpts]
		}
		return fmt.Errorf("anthropic: parse json: %w (got: %s)", err, excerpt)
	}
	return nil
}

// StripFences removes ```json ... ``` or ``` ... ``` wrappers if present and
// trims surrounding whitespace, returning the inner payload.
func StripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence line (``` or ```json).
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	} else {
		s = strings.TrimPrefix(s, "```")
	}
	// Drop the trailing fence.
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
