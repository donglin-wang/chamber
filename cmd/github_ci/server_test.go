package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	chamberCI "github.com/donglin-wang/chamber/internal/ci"
	"github.com/google/uuid"
)

func TestWebhookRejectsNonPushEvents(t *testing.T) {
	server := testServer(t)
	body := []byte(pushPayloadJSON(server.cfg.Repository, false, testSHA()))
	request := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	request.Header.Set("X-GitHub-Event", "pull_request")
	request.Header.Set("X-GitHub-Delivery", "delivery-1")
	request.Header.Set("X-Hub-Signature-256", signedBody(server.cfg.GitHubToken, body))

	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestWebhookRejectsWrongRepository(t *testing.T) {
	server := testServer(t)
	recorder := postWebhook(t, server, pushPayloadJSON("other/repo", false, testSHA()))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestWebhookIgnoresDeletedRefs(t *testing.T) {
	server := testServer(t)
	recorder := postWebhook(t, server, pushPayloadJSON(server.cfg.Repository, true, testSHA()))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
	if len(server.runSlots) != 0 {
		t.Fatalf("run slots = %d, want 0", len(server.runSlots))
	}
}

func TestWebhookReturnsTooManyRequestsWhenSlotBusy(t *testing.T) {
	server := testServer(t)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	server.runCI = func(ctx context.Context, cfg chamberCI.Config) (int, error) {
		close(started)
		<-release
		return 0, nil
	}
	server.checkout = func(context.Context, string, string, string) error {
		return nil
	}
	server.statusClient = &recordingStatusClient{}

	first := postWebhook(t, server, pushPayloadJSON(server.cfg.Repository, false, testSHA()))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusAccepted)
	}
	<-started

	second := postWebhook(t, server, pushPayloadJSON(server.cfg.Repository, false, "1123456789abcdef0123456789abcdef01234567"))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", second.Code, http.StatusTooManyRequests)
	}

	go func() {
		close(release)
		for len(server.runSlots) != 0 {
			time.Sleep(10 * time.Millisecond)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run slot was not released")
	}
}

func TestWebhookRunsCheckoutAndCIAndServesLogs(t *testing.T) {
	server := testServer(t)
	statuses := &recordingStatusClient{}
	server.statusClient = statuses
	ran := make(chan struct{})
	server.checkout = func(ctx context.Context, checkoutDir string, remote string, sha string) error {
		wantRemote := "https://github.com/" + server.cfg.Repository + ".git"
		if remote != wantRemote {
			t.Fatalf("remote = %q, want %q", remote, wantRemote)
		}
		if sha != testSHA() {
			t.Fatalf("sha = %q, want %q", sha, testSHA())
		}
		if filepath.Base(checkoutDir) != "checkout" {
			t.Fatalf("checkout dir = %q, want checkout leaf", checkoutDir)
		}
		return nil
	}
	server.runCI = func(ctx context.Context, cfg chamberCI.Config) (int, error) {
		close(ran)
		return 0, nil
	}

	recorder := postWebhook(t, server, pushPayloadJSON(server.cfg.Repository, false, testSHA()))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	<-ran
	waitForStatuses(t, statuses, 2)

	var admitted struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &admitted); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, err := uuid.Parse(admitted.RunID); err != nil {
		t.Fatalf("run_id = %q, want raw UUID: %v", admitted.RunID, err)
	}
	if statuses.statuses[0].State != statusPending || statuses.statuses[1].State != statusSuccess {
		t.Fatalf("statuses = %#v, want pending then success", statuses.statuses)
	}
}

func testServer(t *testing.T) *server {
	t.Helper()
	cfg := config{
		Root:        t.TempDir(),
		Repository:  "donglin-wang/chamber",
		GitHubToken: "secret",
		MaxParallel: 1,
		RunTimeout:  time.Minute,
		Retention:   time.Hour,
	}
	server := newWebhookServer(cfg)
	server.now = func() time.Time {
		return time.Unix(100, 0)
	}
	server.checkout = func(context.Context, string, string, string) error {
		return nil
	}
	server.runCI = func(context.Context, chamberCI.Config) (int, error) {
		return 0, nil
	}
	server.statusClient = &recordingStatusClient{}
	return server
}

func postWebhook(t *testing.T, server *server, payload string) *httptest.ResponseRecorder {
	t.Helper()
	body := []byte(payload)
	request := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", "delivery-1")
	request.Header.Set("X-Hub-Signature-256", signedBody(server.cfg.GitHubToken, body))
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	return recorder
}

func pushPayloadJSON(repository string, deleted bool, sha string) string {
	return `{"ref":"refs/heads/main","after":"` + sha + `","deleted":` + strconv.FormatBool(deleted) + `,"repository":{"full_name":"` + repository + `"}}`
}

func testSHA() string {
	return "0123456789abcdef0123456789abcdef01234567"
}

type recordingStatusClient struct {
	mu       sync.Mutex
	statuses []githubStatus
}

func (c *recordingStatusClient) CreateStatus(ctx context.Context, sha string, status githubStatus) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statuses = append(c.statuses, status)
	return nil
}

func waitForStatuses(t *testing.T, client *recordingStatusClient, count int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		client.mu.Lock()
		got := len(client.statuses)
		client.mu.Unlock()
		if got >= count {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("statuses = %d, want at least %d", got, count)
		case <-ticker.C:
		}
	}
}
