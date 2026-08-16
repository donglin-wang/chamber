package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const githubAPIBaseURL = "https://api.github.com"
const githubStatusContext = "chamber-ci"

type statusState string

const (
	statusPending statusState = "pending"
	statusSuccess statusState = "success"
	statusFailure statusState = "failure"
	statusError   statusState = "error"
)

type githubStatus struct {
	State       statusState `json:"state"`
	TargetURL   string      `json:"target_url,omitempty"`
	Description string      `json:"description"`
	Context     string      `json:"context"`
}

type githubStatusClient struct {
	client     *http.Client
	repository string
	token      string
}

func newGitHubStatusClient(cfg config) *githubStatusClient {
	return &githubStatusClient{
		client:     http.DefaultClient,
		repository: cfg.Repository,
		token:      cfg.GitHubToken,
	}
}

func (c *githubStatusClient) CreateStatus(ctx context.Context, sha string, status githubStatus) error {
	if c == nil {
		return nil
	}
	body, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("encode GitHub status: %w", err)
	}
	endpoint := fmt.Sprintf("%s/repos/%s/statuses/%s", githubAPIBaseURL, c.repository, url.PathEscape(sha))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create GitHub status request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := c.client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("post GitHub status: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("post GitHub status: unexpected status %s", response.Status)
	}
	return nil
}

func validateSignature(secret string, body []byte, signature string) bool {
	if strings.TrimSpace(secret) == "" || !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

func githubStatusForOutcome(outcome runStatus) githubStatus {
	switch outcome {
	case runStatusSucceeded:
		return githubStatus{
			State:       statusSuccess,
			Description: "Chamber CI passed",
		}
	case runStatusFailed:
		return githubStatus{
			State:       statusFailure,
			Description: "Chamber CI failed",
		}
	case runStatusErrored:
		return githubStatus{
			State:       statusError,
			Description: "Chamber CI errored before tests completed",
		}
	default:
		return githubStatus{
			State:       statusPending,
			Description: "Chamber CI is running",
		}
	}
}
