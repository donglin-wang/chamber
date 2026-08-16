package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseConfigReadsCommandLineConfigAndSecretsFile(t *testing.T) {
	secretsFile := writeSecretsFile(t, `{
		"github_token": "status-token",
		"github_webhook_secret": "webhook-secret"
	}`)

	cfg, err := parseConfig([]string{
		"-addr=0.0.0.0:8080",
		"-root=/home/test/.local/state/chamber-ci",
		"-repository=example/chamber",
		"-status-target-base-url=https://ci.example.test/",
		"-max-parallel=5",
		"-run-timeout=45m",
		"-retention=24h",
		"-skip-preflight",
		"-secrets-file=" + secretsFile,
	})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}

	if cfg.Addr != "0.0.0.0:8080" {
		t.Fatalf("Addr = %q, want 0.0.0.0:8080", cfg.Addr)
	}
	if cfg.Root != "/home/test/.local/state/chamber-ci" {
		t.Fatalf("Root = %q, want /home/test/.local/state/chamber-ci", cfg.Root)
	}
	if cfg.Repository != "example/chamber" {
		t.Fatalf("Repository = %q, want example/chamber", cfg.Repository)
	}
	if cfg.StatusTargetBaseURL != "https://ci.example.test" {
		t.Fatalf("StatusTargetBaseURL = %q, want trimmed URL", cfg.StatusTargetBaseURL)
	}
	if cfg.GitHubToken != "status-token" {
		t.Fatalf("GitHubToken = %q, want status-token", cfg.GitHubToken)
	}
	if cfg.GitHubWebhookSecret != "webhook-secret" {
		t.Fatalf("GitHubWebhookSecret = %q, want webhook-secret", cfg.GitHubWebhookSecret)
	}
	if cfg.MaxParallel != 5 {
		t.Fatalf("MaxParallel = %d, want 5", cfg.MaxParallel)
	}
	if cfg.RunTimeout != 45*time.Minute {
		t.Fatalf("RunTimeout = %s, want 45m", cfg.RunTimeout)
	}
	if cfg.Retention != 24*time.Hour {
		t.Fatalf("Retention = %s, want 24h", cfg.Retention)
	}
	if !cfg.SkipPreflight {
		t.Fatal("SkipPreflight = false, want true")
	}
}

func TestParseConfigRequiresGitHubWebhookSecret(t *testing.T) {
	secretsFile := writeSecretsFile(t, `{"github_token": "status-token"}`)

	_, err := parseConfig([]string{"-secrets-file=" + secretsFile})
	if err == nil {
		t.Fatal("parseConfig() error = nil, want missing webhook secret error")
	}
	if !strings.Contains(err.Error(), "GitHub webhook secret is required") {
		t.Fatalf("parseConfig() error = %v, want missing webhook secret error", err)
	}
}

func TestParseConfigRequiresSecretsFile(t *testing.T) {
	_, err := parseConfig(nil)
	if err == nil {
		t.Fatal("parseConfig() error = nil, want missing secrets file error")
	}
	if !strings.Contains(err.Error(), "secrets file is required") {
		t.Fatalf("parseConfig() error = %v, want missing secrets file error", err)
	}
}

func TestParseConfigRejectsUnknownSecretFields(t *testing.T) {
	secretsFile := writeSecretsFile(t, `{
		"github_token": "status-token",
		"github_webhook_secret": "webhook-secret",
		"extra": "unexpected"
	}`)

	_, err := parseConfig([]string{"-secrets-file=" + secretsFile})
	if err == nil {
		t.Fatal("parseConfig() error = nil, want unknown field error")
	}
	if !strings.Contains(err.Error(), `unknown field "extra"`) {
		t.Fatalf("parseConfig() error = %v, want unknown field error", err)
	}
}

func writeSecretsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "github-ci-secrets.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write secrets file: %v", err)
	}
	return path
}
