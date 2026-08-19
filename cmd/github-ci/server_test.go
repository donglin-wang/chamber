package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWebhookRejectsNonPushEvents(t *testing.T) {
	server := testServer(t)
	body := []byte(pushPayloadJSON(server.cfg.Repository, false, testSHA()))
	request := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	request.Header.Set("X-GitHub-Event", "pull_request")
	request.Header.Set("X-GitHub-Delivery", "delivery-1")
	request.Header.Set("X-Hub-Signature-256", signedBody(server.cfg.GitHubWebhookSecret, body))

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

func TestWebhookRejectsSignatureMadeWithGitHubToken(t *testing.T) {
	server := testServer(t)
	body := []byte(pushPayloadJSON(server.cfg.Repository, false, testSHA()))
	request := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", "delivery-1")
	request.Header.Set("X-Hub-Signature-256", signedBody(server.cfg.GitHubToken, body))

	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
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
	server.runCI = func(ctx context.Context, cfg ciConfig) (int, error) {
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

func TestCancelActiveRunsMarksRunErrored(t *testing.T) {
	server := testServer(t)
	statuses := &recordingStatusClient{}
	server.statusClient = statuses
	started := make(chan struct{})
	server.runCI = func(ctx context.Context, cfg ciConfig) (int, error) {
		close(started)
		<-ctx.Done()
		return 1, ctx.Err()
	}

	recorder := postWebhook(t, server, pushPayloadJSON(server.cfg.Repository, false, testSHA()))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	<-started
	server.cancelActiveRuns()
	waitForStatuses(t, statuses, 2)

	var admitted struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &admitted); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	record, err := readRunRecord(server.cfg.Root, admitted.RunID)
	if err != nil {
		t.Fatalf("read run record: %v", err)
	}
	if record.Status != runStatusErrored {
		t.Fatalf("Status = %q, want errored", record.Status)
	}
	if record.CompletedAt == nil {
		t.Fatal("CompletedAt = nil, want timestamp")
	}
	if statuses.statuses[0].State != statusPending || statuses.statuses[1].State != statusError {
		t.Fatalf("statuses = %#v, want pending then error", statuses.statuses)
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
	server.runCI = func(ctx context.Context, cfg ciConfig) (int, error) {
		for _, writer := range cfg.Stdout {
			_, _ = writer.Write([]byte("go test stdout\n"))
		}
		for _, writer := range cfg.Stderr {
			_, _ = writer.Write([]byte("go test stderr\n"))
		}
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
	wantTargetURL := "https://ci.example.test/runs/" + admitted.RunID + "/logs"
	if statuses.statuses[0].TargetURL != wantTargetURL || statuses.statuses[1].TargetURL != wantTargetURL {
		t.Fatalf("target URLs = %q, %q; want %q", statuses.statuses[0].TargetURL, statuses.statuses[1].TargetURL, wantTargetURL)
	}

	logRequest := httptest.NewRequest(http.MethodGet, "/runs/"+admitted.RunID+"/logs", nil)
	logRecorder := httptest.NewRecorder()
	server.routes().ServeHTTP(logRecorder, logRequest)
	if logRecorder.Code != http.StatusOK {
		t.Fatalf("whole log status = %d, want %d", logRecorder.Code, http.StatusOK)
	}
	wholeLog := logRecorder.Body.String()
	for _, want := range []string{
		"Chamber CI run " + admitted.RunID,
		"Status: succeeded",
		"===== ci stdout =====",
		"go test stdout",
		"===== ci stderr =====",
		"go test stderr",
	} {
		if !strings.Contains(wholeLog, want) {
			t.Fatalf("whole log missing %q:\n%s", want, wholeLog)
		}
	}

	stdoutRequest := httptest.NewRequest(http.MethodGet, "/runs/"+admitted.RunID+"/logs/ci/stdout", nil)
	stdoutRecorder := httptest.NewRecorder()
	server.routes().ServeHTTP(stdoutRecorder, stdoutRequest)
	if stdoutRecorder.Code != http.StatusOK || stdoutRecorder.Body.String() != "go test stdout\n" {
		t.Fatalf("stdout log status/body = %d/%q, want 200/go test stdout", stdoutRecorder.Code, stdoutRecorder.Body.String())
	}
}

func TestRecoverIncompleteRunsMarksRunningRecordErrored(t *testing.T) {
	server := testServer(t)
	statuses := &recordingStatusClient{}
	server.statusClient = statuses
	runID := uuid.NewString()
	startedAt := time.Unix(50, 0).UTC()
	if err := writeRunRecord(server.cfg.Root, runRecord{
		RunID:      runID,
		SHA:        testSHA(),
		Ref:        "refs/heads/main",
		Repository: server.cfg.Repository,
		Status:     runStatusRunning,
		StartedAt:  startedAt,
		Logs:       runLogLinks(runID),
	}); err != nil {
		t.Fatalf("write run record: %v", err)
	}

	if err := server.recoverIncompleteRuns(context.Background()); err != nil {
		t.Fatalf("recoverIncompleteRuns() error = %v", err)
	}

	record, err := readRunRecord(server.cfg.Root, runID)
	if err != nil {
		t.Fatalf("read run record: %v", err)
	}
	if record.Status != runStatusErrored {
		t.Fatalf("Status = %q, want errored", record.Status)
	}
	if record.ExitCode != 1 {
		t.Fatalf("ExitCode = %d, want 1", record.ExitCode)
	}
	if record.CompletedAt == nil || !record.CompletedAt.Equal(time.Unix(100, 0).UTC()) {
		t.Fatalf("CompletedAt = %v, want fixed recovery time", record.CompletedAt)
	}
	if !strings.Contains(record.Error, "startup recovery") {
		t.Fatalf("Error = %q, want recovery message", record.Error)
	}
	if len(statuses.statuses) != 1 || statuses.statuses[0].State != statusError {
		t.Fatalf("statuses = %#v, want one error status", statuses.statuses)
	}
	wantTargetURL := "https://ci.example.test/runs/" + runID + "/logs"
	if statuses.statuses[0].TargetURL != wantTargetURL {
		t.Fatalf("TargetURL = %q, want %q", statuses.statuses[0].TargetURL, wantTargetURL)
	}
}

func TestRecoverIncompleteRunsCreatesErroredRecordForUnreadableRecord(t *testing.T) {
	server := testServer(t)
	statuses := &recordingStatusClient{}
	server.statusClient = statuses
	runID := uuid.NewString()
	runDir := filepath.Join(server.cfg.Root, "runs", runID)
	if err := os.MkdirAll(runDir, 0700); err != nil {
		t.Fatalf("create run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "status.json"), []byte("{"), 0600); err != nil {
		t.Fatalf("write unreadable status: %v", err)
	}

	if err := server.recoverIncompleteRuns(context.Background()); err != nil {
		t.Fatalf("recoverIncompleteRuns() error = %v", err)
	}

	record, err := readRunRecord(server.cfg.Root, runID)
	if err != nil {
		t.Fatalf("read recovered run record: %v", err)
	}
	if record.Status != runStatusErrored {
		t.Fatalf("Status = %q, want errored", record.Status)
	}
	if !strings.Contains(record.Error, "did not complete before startup recovery") {
		t.Fatalf("Error = %q, want incomplete recovery message", record.Error)
	}
	if len(statuses.statuses) != 0 {
		t.Fatalf("statuses = %#v, want no GitHub status without SHA", statuses.statuses)
	}
}

func TestPruneOldRunsSkipsRunningAndUnreadableRecords(t *testing.T) {
	root := t.TempDir()
	old := time.Unix(10, 0)
	olderThan := time.Unix(20, 0)
	completedID := uuid.NewString()
	runningID := uuid.NewString()
	unreadableID := uuid.NewString()
	completedAt := old.UTC()
	for _, record := range []runRecord{
		{
			RunID:       completedID,
			SHA:         testSHA(),
			Status:      runStatusSucceeded,
			StartedAt:   old.UTC(),
			CompletedAt: &completedAt,
		},
		{
			RunID:     runningID,
			SHA:       testSHA(),
			Status:    runStatusRunning,
			StartedAt: old.UTC(),
		},
	} {
		if err := writeRunRecord(root, record); err != nil {
			t.Fatalf("write run record %s: %v", record.RunID, err)
		}
	}
	unreadableDir := filepath.Join(root, "runs", unreadableID)
	if err := os.MkdirAll(unreadableDir, 0700); err != nil {
		t.Fatalf("create unreadable run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(unreadableDir, "status.json"), []byte("{"), 0600); err != nil {
		t.Fatalf("write unreadable status: %v", err)
	}
	for _, runID := range []string{completedID, runningID, unreadableID} {
		if err := os.Chtimes(filepath.Join(root, "runs", runID), old, old); err != nil {
			t.Fatalf("set run dir time: %v", err)
		}
	}

	if err := pruneOldRuns(root, olderThan); err != nil {
		t.Fatalf("pruneOldRuns() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "runs", completedID)); !os.IsNotExist(err) {
		t.Fatalf("completed run exists error = %v, want removed", err)
	}
	for _, runID := range []string{runningID, unreadableID} {
		if _, err := os.Stat(filepath.Join(root, "runs", runID)); err != nil {
			t.Fatalf("run %s stat error = %v, want retained", runID, err)
		}
	}
}

func testServer(t *testing.T) *server {
	t.Helper()
	cfg := config{
		Root:                t.TempDir(),
		StatusTargetBaseURL: "https://ci.example.test",
		Repository:          "donglin-wang/chamber",
		GitHubToken:         "status-token",
		GitHubWebhookSecret: "webhook-secret",
		MaxParallel:         1,
		RunTimeout:          time.Minute,
		Retention:           time.Hour,
	}
	server := newWebhookServer(cfg)
	server.now = func() time.Time {
		return time.Unix(100, 0)
	}
	server.checkout = func(context.Context, string, string, string) error {
		return nil
	}
	server.runCI = func(context.Context, ciConfig) (int, error) {
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
	request.Header.Set("X-Hub-Signature-256", signedBody(server.cfg.GitHubWebhookSecret, body))
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
