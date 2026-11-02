package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
)

type config struct {
	Listen       string                      `json:"listen"`
	PublicURL    string                      `json:"public_url"`
	Root         string                      `json:"root"`
	WorkRoot     string                      `json:"work_root"`
	MaxParallel  int                         `json:"max_parallel"`
	MaxQueued    int                         `json:"max_queued"`
	JobTimeout   string                      `json:"job_timeout"`
	MaxLogBytes  int64                       `json:"max_log_bytes"`
	Repositories map[string]repository       `json:"repositories"`
	Update       updateSource                `json:"update"`
	BuildKit     chamberImage.BuildKitConfig `json:"buildkit"`
	RuntimePath  string                      `json:"runtime_path"`
	jobTimeout   time.Duration
}

type repository struct {
	Name      string `json:"name"`
	CloneURL  string `json:"clone_url"`
	APIURL    string `json:"api_url"`
	TokenEnv  string `json:"token_env"`
	SecretEnv string `json:"secret_env"`
	token     string
	secret    string
}

type updateSource struct {
	CloneURL string `json:"clone_url"`
	Ref      string `json:"ref"`
}

var routeName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
var repositoryName = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func loadConfig(path string) (config, error) {
	c := config{Listen: "127.0.0.1:8080", MaxParallel: 1, MaxQueued: 64,
		JobTimeout: "30m", MaxLogBytes: 16 << 20,
		Update: updateSource{CloneURL: "https://github.com/donglin-wang/chamber.git", Ref: "refs/heads/main"}}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return c, fmt.Errorf("config must contain one JSON object")
	}
	if !filepath.IsAbs(c.Root) || filepath.Clean(c.Root) == "/" {
		return c, fmt.Errorf("root must be an absolute private directory")
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return c, fmt.Errorf("public_url must be an http(s) origin without credentials, path, query, or fragment")
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.WorkRoot == "" {
		c.WorkRoot = fmt.Sprintf("/tmp/chamber-ci-%d", os.Geteuid())
	}
	if !filepath.IsAbs(c.WorkRoot) || filepath.Clean(c.WorkRoot) == "/" {
		return c, fmt.Errorf("work_root must be an absolute private directory")
	}
	c.Root, c.WorkRoot = filepath.Clean(c.Root), filepath.Clean(c.WorkRoot)
	c.jobTimeout, err = time.ParseDuration(c.JobTimeout)
	if err != nil || c.jobTimeout <= 0 || c.MaxParallel < 1 || c.MaxQueued < 1 || c.MaxLogBytes < 1024 || c.Listen == "" {
		return c, fmt.Errorf("invalid limits, job_timeout, or listen address")
	}
	if len(c.Repositories) == 0 {
		return c, fmt.Errorf("at least one repository is required")
	}
	for key, r := range c.Repositories {
		if !routeName.MatchString(key) || !repositoryName.MatchString(r.Name) {
			return c, fmt.Errorf("invalid repository key or name: %q", key)
		}
		if r.CloneURL == "" {
			r.CloneURL = "https://github.com/" + r.Name + ".git"
		}
		if err := validateCloneURL(r.CloneURL); err != nil {
			return c, fmt.Errorf("repository %s: %w", key, err)
		}
		if r.APIURL == "" {
			r.APIURL = "https://api.github.com"
		}
		u, err := url.Parse(r.APIURL)
		if err != nil || u.Host == "" || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return c, fmt.Errorf("repository %s: api_url must use HTTPS without credentials or query", key)
		}
		r.APIURL = strings.TrimRight(r.APIURL, "/")
		r.token, r.secret = os.Getenv(r.TokenEnv), os.Getenv(r.SecretEnv)
		if r.token == "" || r.secret == "" {
			return c, fmt.Errorf("repository %s: token_env and secret_env must name nonempty environment variables", key)
		}
		c.Repositories[key] = r
	}
	if err := validateCloneURL(c.Update.CloneURL); err != nil {
		return c, fmt.Errorf("update: %w", err)
	}
	if !strings.HasPrefix(c.Update.Ref, "refs/heads/") || strings.ContainsAny(c.Update.Ref, " \t\n\r~^:?*[\\") || strings.Contains(c.Update.Ref, "..") || strings.HasSuffix(c.Update.Ref, "/") {
		return c, fmt.Errorf("update.ref must be a full branch ref")
	}
	return c, nil
}

func validateCloneURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "ssh") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("clone_url must be an HTTPS or ssh:// URL")
	}
	if u.User != nil {
		if _, password := u.User.Password(); password || u.Scheme != "ssh" {
			return fmt.Errorf("clone_url must not contain credentials; use Git credentials or SSH keys")
		}
	}
	return nil
}
