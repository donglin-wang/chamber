package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
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
	GitHubToken         string
	MaxParallel         int
	RunTimeout          time.Duration
	Retention           time.Duration
	SkipPreflight       bool
}

func parseConfig(args []string, getenv func(string) string) (config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	envString := func(key string, fallback string) string {
		if value := strings.TrimSpace(getenv(key)); value != "" {
			return value
		}
		return fallback
	}
	envInt := func(key string, fallback int) int {
		value := strings.TrimSpace(getenv(key))
		if value == "" {
			return fallback
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fallback
		}
		return parsed
	}
	envDuration := func(key string, fallback time.Duration) time.Duration {
		value := strings.TrimSpace(getenv(key))
		if value == "" {
			return fallback
		}
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fallback
		}
		return parsed
	}
	envBool := func(key string, fallback bool) bool {
		value := strings.TrimSpace(getenv(key))
		if value == "" {
			return fallback
		}
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fallback
		}
		return parsed
	}

	cfg := config{
		Addr:                envString("CHAMBER_CI_ADDR", defaultAddr),
		StatusTargetBaseURL: strings.TrimRight(getenv("CHAMBER_CI_STATUS_TARGET_BASE_URL"), "/"),
		Root:                envString("CHAMBER_CI_ROOT", defaultRoot),
		Repository:          envString("CHAMBER_CI_REPOSITORY", defaultRepository),
		GitHubToken:         getenv("CHAMBER_CI_GITHUB_TOKEN"),
		MaxParallel:         envInt("MAX_PARALLEL", defaultMaxParallel),
		RunTimeout:          envDuration("CHAMBER_CI_RUN_TIMEOUT", defaultRunTimeout),
		Retention:           envDuration("CHAMBER_CI_RETENTION", defaultRetention),
		SkipPreflight:       envBool("CHAMBER_CI_SKIP_PREFLIGHT", false),
	}

	flags := flag.NewFlagSet("github-ci", flag.ContinueOnError)
	flags.StringVar(&cfg.Addr, "addr", cfg.Addr, "HTTP listen address")
	flags.StringVar(&cfg.StatusTargetBaseURL, "status-target-base-url", cfg.StatusTargetBaseURL, "base URL for GitHub commit status target links")
	flags.StringVar(&cfg.Root, "root", cfg.Root, "root directory for all GitHub CI mutable state")
	flags.StringVar(&cfg.Repository, "repository", cfg.Repository, "allowed GitHub repository full name")
	flags.StringVar(&cfg.GitHubToken, "github-token", cfg.GitHubToken, "GitHub token used for webhook verification and commit statuses")
	flags.IntVar(&cfg.MaxParallel, "max-parallel", cfg.MaxParallel, "maximum admitted CI runs in this process")
	flags.DurationVar(&cfg.RunTimeout, "run-timeout", cfg.RunTimeout, "timeout for one CI run")
	flags.DurationVar(&cfg.Retention, "retention", cfg.Retention, "duration to keep run directories")
	flags.BoolVar(&cfg.SkipPreflight, "skip-preflight", cfg.SkipPreflight, "skip startup host preflight")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	cfg.StatusTargetBaseURL = strings.TrimRight(cfg.StatusTargetBaseURL, "/")
	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func (cfg config) validate() error {
	for label, value := range map[string]string{
		"addr":         cfg.Addr,
		"root":         cfg.Root,
		"repository":   cfg.Repository,
		"GitHub token": cfg.GitHubToken,
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
