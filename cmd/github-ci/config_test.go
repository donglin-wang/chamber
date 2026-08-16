package main

import (
	"strings"
	"testing"
)

func TestParseConfigSeparatesGitHubTokenAndWebhookSecret(t *testing.T) {
	env := map[string]string{
		"CHAMBER_CI_GITHUB_TOKEN":          "status-token",
		"CHAMBER_CI_GITHUB_WEBHOOK_SECRET": "webhook-secret",
	}
	cfg, err := parseConfig(nil, func(key string) string {
		return env[key]
	})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}

	if cfg.GitHubToken != "status-token" {
		t.Fatalf("GitHubToken = %q, want status-token", cfg.GitHubToken)
	}
	if cfg.GitHubWebhookSecret != "webhook-secret" {
		t.Fatalf("GitHubWebhookSecret = %q, want webhook-secret", cfg.GitHubWebhookSecret)
	}
}

func TestParseConfigRequiresGitHubWebhookSecret(t *testing.T) {
	env := map[string]string{
		"CHAMBER_CI_GITHUB_TOKEN": "status-token",
	}
	_, err := parseConfig(nil, func(key string) string {
		return env[key]
	})
	if err == nil {
		t.Fatal("parseConfig() error = nil, want missing webhook secret error")
	}
	if !strings.Contains(err.Error(), "GitHub webhook secret is required") {
		t.Fatalf("parseConfig() error = %v, want missing webhook secret error", err)
	}
}
