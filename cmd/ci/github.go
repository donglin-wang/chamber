package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type githubEvent struct {
	After      string `json:"after"`
	Ref        string `json:"ref"`
	Deleted    bool   `json:"deleted"`
	Action     string `json:"action"`
	Number     int    `json:"number"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}

func verifySignature(body []byte, signature, secret string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}

// Only configured clone/API locations are trusted, never URLs from a webhook.
func parseEvent(kind string, body []byte, repo repository) (sha, ref string, err error) {
	if kind == "ping" {
		return "", "", nil
	}
	if kind != "push" && kind != "pull_request" {
		return "", "", nil
	}
	var e githubEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return "", "", err
	}
	if !strings.EqualFold(e.Repository.FullName, repo.Name) {
		return "", "", fmt.Errorf("repository does not match configured webhook")
	}
	switch kind {
	case "push":
		if e.Deleted || !strings.HasPrefix(e.Ref, "refs/heads/") {
			return "", "", nil
		}
		sha, ref = e.After, e.Ref
	case "pull_request":
		if e.Action != "opened" && e.Action != "synchronize" && e.Action != "reopened" && e.Action != "ready_for_review" {
			return "", "", nil
		}
		if e.Number <= 0 {
			return "", "", fmt.Errorf("missing pull request number")
		}
		sha, ref = e.PullRequest.Head.SHA, fmt.Sprintf("refs/pull/%d/head", e.Number)
	}
	if !commitSHA.MatchString(sha) || sha == strings.Repeat("0", 40) {
		return "", "", fmt.Errorf("missing or invalid commit SHA")
	}
	return sha, ref, nil
}

func reportStatus(ctx context.Context, client *http.Client, repo repository, j job, publicURL string) error {
	state := j.State
	if !j.terminal() {
		state = "pending"
	}
	data, err := json.Marshal(map[string]string{
		"state": state, "context": "chamber", "description": j.Message,
		"target_url": publicURL + "/runs/" + j.ID,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, repo.APIURL+"/repos/"+repo.Name+"/statuses/"+j.SHA, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+repo.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub status request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub status HTTP %d", resp.StatusCode)
	}
	return nil
}
