package config

import (
	"strings"
	"testing"
)

func setRequired(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"LLM_API_KEY":         "k",
		"GITLAB_URL":          "https://gitlab.example.com/",
		"GITLAB_TOKEN":        "t",
		"GITLAB_PROJECT_ID":   "42",
		"OPENSEARCH_URL":      "https://os.example.com",
		"OPENSEARCH_USERNAME": "u",
		"OPENSEARCH_PASSWORD": "p",
	} {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitLabURL != "https://gitlab.example.com" {
		t.Errorf("trailing slash not trimmed: %q", cfg.GitLabURL)
	}
	if cfg.LLMModel != "anthropic/claude-sonnet-5.5" || cfg.LLMBaseURL != "https://openrouter.ai/api/v1" || cfg.LLMProvider != "openrouter" || cfg.OpenSearchLevelField != "level" || cfg.OpenSearchStackTraceField != "stack_trace" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if !cfg.HumanApprovalRequired || cfg.OpenSearchPollInterval != 60 || cfg.Port != "3000" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadReportsAllMissing(t *testing.T) {
	for _, k := range []string{"LLM_API_KEY", "GITLAB_URL", "GITLAB_TOKEN", "GITLAB_PROJECT_ID",
		"OPENSEARCH_URL", "OPENSEARCH_USERNAME", "OPENSEARCH_PASSWORD"} {
		t.Setenv(k, "")
	}
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, k := range []string{"LLM_API_KEY", "GITLAB_TOKEN", "OPENSEARCH_PASSWORD"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error should list %s: %v", k, err)
		}
	}
}

func TestLoadRejectsBadNumbers(t *testing.T) {
	setRequired(t)
	t.Setenv("OPENSEARCH_POLL_INTERVAL_SECONDS", "-5")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for negative poll interval")
	}
}

func TestLoadAvalAIPresetAndOverrides(t *testing.T) {
	setRequired(t)
	t.Setenv("LLM_PROVIDER", "AvalAI")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLMBaseURL != "https://api.avalai.ir/v1" || cfg.LLMModel != "claude-sonnet-5-5" {
		t.Errorf("unexpected avalai preset: %s %s", cfg.LLMBaseURL, cfg.LLMModel)
	}

	t.Setenv("LLM_BASE_URL", "https://gw.example.com/v1/")
	t.Setenv("LLM_MODEL", "my-model")
	t.Setenv("LLM_REASONING_EFFORT", "none")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLMBaseURL != "https://gw.example.com/v1" || cfg.LLMModel != "my-model" || cfg.LLMReasoningEffort != "" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestLoadRejectsBadLLMSettings(t *testing.T) {
	setRequired(t)
	t.Setenv("LLM_PROVIDER", "anthropic")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unknown provider")
	}
	t.Setenv("LLM_PROVIDER", "openrouter")
	t.Setenv("LLM_REASONING_EFFORT", "extreme")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid reasoning effort")
	}
	t.Setenv("LLM_REASONING_EFFORT", "")
	t.Setenv("LLM_PROVIDER", "custom")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "LLM_BASE_URL") {
		t.Fatalf("custom provider without base URL should fail, got %v", err)
	}
}
