package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultAddr        = "127.0.0.1:8080"
	defaultRoot        = "/var/lib/chamber-ci"
	defaultRepository  = "donglin-wang/chamber"
	defaultMaxParallel = 1
	defaultRunTimeout  = 30 * time.Minute
	defaultRetention   = 168 * time.Hour
)

type config struct {
	Addr                string
	StatusTargetBaseURL string
	Root                string
	Repository          string
	SecretsFile         string
	GitHubToken         string
	GitHubWebhookSecret string
	MaxParallel         int
	RunTimeout          time.Duration
	Retention           time.Duration
	SkipPreflight       bool
}

type githubCISecrets struct {
	GitHubToken         string `json:"github_token"`
	GitHubWebhookSecret string `json:"github_webhook_secret"`
}

func parseConfig(args []string) (config, error) {
	cfg := config{
		Addr:        defaultAddr,
		Root:        defaultRoot,
		Repository:  defaultRepository,
		MaxParallel: defaultMaxParallel,
		RunTimeout:  defaultRunTimeout,
		Retention:   defaultRetention,
	}

	flags := flag.NewFlagSet("github-ci", flag.ContinueOnError)
	flags.StringVar(&cfg.Addr, "addr", cfg.Addr, "HTTP listen address")
	flags.StringVar(&cfg.StatusTargetBaseURL, "status-target-base-url", cfg.StatusTargetBaseURL, "base URL for GitHub commit status target links")
	flags.StringVar(&cfg.Root, "root", cfg.Root, "root directory for all GitHub CI mutable state")
	flags.StringVar(&cfg.Repository, "repository", cfg.Repository, "allowed GitHub repository full name")
	flags.StringVar(&cfg.SecretsFile, "secrets-file", cfg.SecretsFile, "JSON file containing GitHub CI secrets")
	flags.IntVar(&cfg.MaxParallel, "max-parallel", cfg.MaxParallel, "maximum admitted CI runs in this process")
	flags.DurationVar(&cfg.RunTimeout, "run-timeout", cfg.RunTimeout, "timeout for one CI run")
	flags.DurationVar(&cfg.Retention, "retention", cfg.Retention, "duration to keep run directories")
	flags.BoolVar(&cfg.SkipPreflight, "skip-preflight", cfg.SkipPreflight, "skip startup host preflight")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	cfg.StatusTargetBaseURL = strings.TrimRight(cfg.StatusTargetBaseURL, "/")
	if err := cfg.validatePaths(); err != nil {
		return config{}, err
	}
	if err := cfg.loadSecrets(); err != nil {
		return config{}, err
	}
	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func (cfg *config) loadSecrets() error {
	if strings.TrimSpace(cfg.SecretsFile) == "" {
		return fmt.Errorf("secrets file is required")
	}
	file, err := os.Open(cfg.SecretsFile)
	if err != nil {
		return fmt.Errorf("open secrets file: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var secrets githubCISecrets
	if err := decoder.Decode(&secrets); err != nil {
		return fmt.Errorf("decode secrets file: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("decode secrets file: secrets file must contain one JSON object")
	}
	cfg.GitHubToken = secrets.GitHubToken
	cfg.GitHubWebhookSecret = secrets.GitHubWebhookSecret
	return nil
}

func (cfg config) validatePaths() error {
	for _, path := range []struct {
		label string
		value string
	}{
		{label: "root", value: cfg.Root},
		{label: "secrets file", value: cfg.SecretsFile},
	} {
		if strings.TrimSpace(path.value) == "" {
			return fmt.Errorf("%s is required", path.label)
		}
		if !filepath.IsAbs(path.value) {
			return fmt.Errorf("%s must be an absolute path", path.label)
		}
	}
	return nil
}

func (cfg config) validate() error {
	for label, value := range map[string]string{
		"addr":                  cfg.Addr,
		"repository":            cfg.Repository,
		"GitHub token":          cfg.GitHubToken,
		"GitHub webhook secret": cfg.GitHubWebhookSecret,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if cfg.MaxParallel < 1 {
		return fmt.Errorf("max parallel must be at least 1")
	}
	if cfg.RunTimeout <= 0 {
		return fmt.Errorf("run timeout must be positive")
	}
	if cfg.Retention < 0 {
		return fmt.Errorf("retention must not be negative")
	}
	return nil
}
