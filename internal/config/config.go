package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds all runtime configuration loaded from the environment.
type Config struct {
	// Anthropic
	AnthropicAPIKey  string
	AnthropicBaseURL string

	// GitLab
	GitLabURL           string
	GitLabToken         string
	GitLabProjectID     string
	GitLabDefaultBranch string
	GitLabMRTarget      string
	GitLabMRAssigneeID  int // 0 means unset

	// OpenSearch
	OpenSearchURL          string
	OpenSearchUsername     string
	OpenSearchPassword     string
	OpenSearchIndex        string
	OpenSearchPollInterval int
	OpenSearchLookback     int

	// Redis
	RedisURL string

	// App
	Port                  string
	HumanApprovalRequired bool
	LogLevel              string

	// DBPath is where the SQLite file lives (not in the spec env list; sensible default).
	DBPath string
}

// Load reads the .env file (if present) and the process environment, then
// validates that all required fields are set. Returns a descriptive error
// listing every missing/invalid field so the operator can fix them in one pass.
func Load() (*Config, error) {
	// .env is optional; real env vars take precedence and may already be set.
	_ = godotenv.Load()

	var missing []string

	required := func(key string) string {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}

	optional := func(key, def string) string {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			return def
		}
		return v
	}

	cfg := &Config{
		AnthropicAPIKey:  required("ANTHROPIC_API_KEY"),
		AnthropicBaseURL: optional("ANTHROPIC_BASE_URL", "https://api.anthropic.com"),

		GitLabURL:           strings.TrimRight(required("GITLAB_URL"), "/"),
		GitLabToken:         required("GITLAB_TOKEN"),
		GitLabProjectID:     required("GITLAB_PROJECT_ID"),
		GitLabDefaultBranch: optional("GITLAB_DEFAULT_BRANCH", "main"),
		GitLabMRTarget:      optional("GITLAB_MR_TARGET_BRANCH", "main"),

		OpenSearchURL:      strings.TrimRight(required("OPENSEARCH_URL"), "/"),
		OpenSearchUsername: required("OPENSEARCH_USERNAME"),
		OpenSearchPassword: required("OPENSEARCH_PASSWORD"),
		OpenSearchIndex:    optional("OPENSEARCH_INDEX", "logs-*"),

		RedisURL: optional("REDIS_URL", "redis://localhost:6379"),

		Port:     optional("PORT", "3000"),
		LogLevel: optional("LOG_LEVEL", "info"),
		DBPath:   optional("DB_PATH", "logai.db"),
	}

	// Numeric / boolean parsing with validation.
	var err error

	cfg.OpenSearchPollInterval, err = parseIntDefault("OPENSEARCH_POLL_INTERVAL_SECONDS", 60)
	if err != nil {
		return nil, err
	}
	cfg.OpenSearchLookback, err = parseIntDefault("OPENSEARCH_LOOKBACK_SECONDS", 120)
	if err != nil {
		return nil, err
	}

	if v := strings.TrimSpace(os.Getenv("GITLAB_MR_ASSIGNEE_ID")); v != "" {
		cfg.GitLabMRAssigneeID, err = strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("config: GITLAB_MR_ASSIGNEE_ID must be an integer: %w", err)
		}
	}

	cfg.HumanApprovalRequired = parseBoolDefault("HUMAN_APPROVAL_REQUIRED", true)

	if len(missing) > 0 {
		return nil, fmt.Errorf("config: missing required environment variables: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}

func parseIntDefault(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer: %w", key, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("config: %s must be a positive integer", key)
	}
	return n, nil
}

func parseBoolDefault(key string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}
