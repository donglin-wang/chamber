package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testServer(t *testing.T) *server {
	t.Helper()
	c := config{Root: t.TempDir(), PublicURL: "https://ci.example.test", MaxParallel: 1, MaxQueued: 2, MaxLogBytes: 4096, jobTimeout: time.Second,
		Repositories: map[string]repository{"test": {Name: "owner/repo", secret: "webhook-secret", token: "github-token"}}}
	if err := os.Chmod(c.Root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"logs"} {
		if err := os.MkdirAll(filepath.Join(c.Root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := openSQLiteStore(filepath.Join(c.Root, "jobs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s, err := newServer(c, "test-passcode", store)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func signedRequest(body, kind string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/test", strings.NewReader(body))
	mac := hmac.New(sha256.New, []byte("webhook-secret"))
	mac.Write([]byte(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	r.Header.Set("X-GitHub-Event", kind)
	return r
}

func pushBody(sha string) string {
	return fmt.Sprintf(`{"repository":{"full_name":"owner/repo","clone_url":"https://attacker.invalid/repo"},"ref":"refs/heads/main","after":%q}`, sha)
}

func admit(t *testing.T, s *server, sha string) job {
	t.Helper()
	w := httptest.NewRecorder()
	routes(s).ServeHTTP(w, signedRequest(pushBody(sha), "push"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("admit: %d %s", w.Code, w.Body.String())
	}
	var j job
	if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	return j
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestWebhookAdmission(t *testing.T) {
	s := testServer(t)
	sha := strings.Repeat("a", 40)
	j := admit(t, s, sha)
	if j.State != "queued" || j.ID != jobID("test", sha) {
		t.Fatalf("unexpected job: %+v", j)
	}
	stored, err := s.store.Get(context.Background(), j.ID)
	if err != nil || stored.ID != j.ID {
		t.Fatalf("admission not durable: %v", err)
	}
	w := httptest.NewRecorder()
	routes(s).ServeHTTP(w, signedRequest(pushBody(sha), "push"))
	if w.Code != http.StatusOK {
		t.Fatalf("duplicate: %d", w.Code)
	}
	admit(t, s, strings.Repeat("b", 40))
	w = httptest.NewRecorder()
	routes(s).ServeHTTP(w, signedRequest(pushBody(strings.Repeat("c", 40)), "push"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("queue limit: %d", w.Code)
	}
	s.accepting = false
	w = httptest.NewRecorder()
	routes(s).ServeHTTP(w, signedRequest(pushBody(strings.Repeat("c", 40)), "push"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("drain: %d", w.Code)
	}
}

func TestWebhookValidation(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, body, kind, signature string
		want                        int
	}{
		{"signature", pushBody(sha), "push", "bad", 401},
		{"repository", strings.ReplaceAll(pushBody(sha), "owner/repo", "other/repo"), "push", "", 400},
		{"invalid SHA", pushBody("HEAD"), "push", "", 400},
		{"deleted", `{"repository":{"full_name":"owner/repo"},"deleted":true}`, "push", "", 204},
		{"tag", `{"repository":{"full_name":"owner/repo"},"ref":"refs/tags/v1"}`, "push", "", 204},
		{"ping", `{}`, "ping", "", 204},
		{"unrelated", `{}`, "issues", "", 204},
		{"malformed", `{`, "push", "", 400},
		{"pull request", fmt.Sprintf(`{"repository":{"full_name":"owner/repo"},"action":"synchronize","number":7,"pull_request":{"head":{"sha":%q}}}`, sha), "pull_request", "", 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t)
			r := signedRequest(tc.body, tc.kind)
			if tc.signature != "" {
				r.Header.Set("X-Hub-Signature-256", tc.signature)
			}
			w := httptest.NewRecorder()
			routes(s).ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestWorkerCompletionPanicAndTimeout(t *testing.T) {
	for _, mode := range []string{"success", "failure", "panic", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t)
			s.config.jobTimeout = 50 * time.Millisecond
			s.execute = func(ctx context.Context, _ config, _ job, w io.Writer) (string, error) {
				fmt.Fprintln(w, "live <script>container output</script>")
				switch mode {
				case "panic":
					panic("lost worker")
				case "timeout":
					<-ctx.Done()
					return "error", ctx.Err()
				case "failure":
					return "failure", errors.New("exit 9")
				}
				return "success", nil
			}
			j := admit(t, s, strings.Repeat("a", 40))
			stop := startDispatcher(t, s)
			eventually(t, func() bool { return storedJob(t, s, j.ID).terminal() })
			stop()
			want := mode
			if mode == "panic" || mode == "timeout" {
				want = "error"
			}
			if storedJob(t, s, j.ID).State != want {
				t.Fatalf("state = %s", storedJob(t, s, j.ID).State)
			}
			db, err := openSQLiteStore(filepath.Join(s.config.Root, "jobs.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			stored, err := db.Get(context.Background(), j.ID)
			db.Close()
			if err != nil || stored.State != want {
				t.Fatalf("not durable: %v", err)
			}
			w := httptest.NewRecorder()
			routes(s).ServeHTTP(w, httptest.NewRequest("GET", "/runs/"+j.ID+"/log", nil))
			if !strings.Contains(w.Body.String(), "container output") || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Fatalf("log: %s", w.Body.String())
			}
			w = httptest.NewRecorder()
			routes(s).ServeHTTP(w, httptest.NewRequest("GET", "/runs/"+j.ID, nil))
			if strings.Contains(w.Body.String(), "live <script>") || !strings.Contains(w.Body.String(), "textContent") {
				t.Fatal("unsafe log viewer")
			}
		})
	}
}

func TestRestartMarksDroppedJobErrorAndRetriesStatus(t *testing.T) {
	s := testServer(t)
	j := admit(t, s, strings.Repeat("a", 40))
	claimed, err := s.store.Claim(context.Background(), 1)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.store.Acknowledge(context.Background(), j.ID, claimed.Version); err != nil {
		t.Fatal(err)
	}
	work := jobWorkspace(s.config, j.ID)
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "leftover"), []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/statuses/"+j.SHA || r.Header.Get("Authorization") != "Bearer github-token" {
			t.Error("bad status request")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["state"] != "error" || body["target_url"] != s.config.PublicURL+"/runs/"+j.ID {
			t.Errorf("bad status: %v", body)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(201)
		}
	}))
	defer github.Close()
	r := s.config.Repositories["test"]
	r.APIURL = github.URL
	s.config.Repositories["test"] = r
	restarted, err := newServer(s.config, s.passcode, s.store)
	if err != nil {
		t.Fatal(err)
	}
	if storedJob(t, restarted, j.ID).State != "error" || storedJob(t, restarted, j.ID).Reported {
		t.Fatal("dropped job not reconciled")
	}
	restarted.reportJobUpdates(context.Background())
	if storedJob(t, restarted, j.ID).Reported {
		t.Fatal("failed status was acknowledged")
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("workspace remains: %v", err)
	}
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSQLiteStore(filepath.Join(s.config.Root, "jobs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	restarted, err = newServer(s.config, s.passcode, reopened)
	if err != nil {
		t.Fatal(err)
	}
	restarted.reportJobUpdates(context.Background())
	if !storedJob(t, restarted, j.ID).Reported || calls.Load() != 2 {
		t.Fatal("status not retried across restart")
	}
	restarted.reportJobUpdates(context.Background())
	if calls.Load() != 2 {
		t.Fatal("acknowledged terminal status sent again")
	}
}

func TestStatusCannotAcknowledgeAChangedOutcome(t *testing.T) {
	s := testServer(t)
	j := admit(t, s, strings.Repeat("a", 40))
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var states []string
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		states = append(states, body["state"])
		mu.Unlock()
		if body["state"] == "pending" {
			close(started)
			<-release
		}
		w.WriteHeader(201)
	}))
	defer github.Close()
	r := s.config.Repositories["test"]
	r.APIURL = github.URL
	s.config.Repositories["test"] = r
	done := make(chan struct{})
	go func() { defer close(done); s.reportJobUpdates(context.Background()) }()
	<-started
	if _, err := s.store.Claim(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.store.Finish(context.Background(), j.ID, "success", "passed"); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if storedJob(t, s, j.ID).Reported {
		t.Fatal("stale pending acknowledged as terminal")
	}
	s.reportJobUpdates(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(states, ",") != "pending,success" {
		t.Fatal(states)
	}
}

func TestUpdateAuthenticationAndExclusion(t *testing.T) {
	s := testServer(t)
	release := make(chan struct{})
	s.buildUpdate = func(context.Context) (string, error) { <-release; return "", errors.New("build failed") }
	request := func(auth string) int {
		r := httptest.NewRequest("POST", "/update", nil)
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		routes(s).ServeHTTP(w, r)
		return w.Code
	}
	if request("Bearer wrong") != 401 || request(s.passcode) != 401 {
		t.Fatal("unauthenticated update")
	}
	if request("Bearer "+s.passcode) != 202 || request("Bearer "+s.passcode) != 409 {
		t.Fatal("concurrent updates accepted")
	}
	close(release)
	s.tasks.Wait()
	if !s.accepting || s.updating {
		t.Fatal("build failure stopped admission")
	}
}

func TestRunningJobLogsAndAdmissionLimit(t *testing.T) {
	s := testServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.execute = func(ctx context.Context, _ config, _ job, output io.Writer) (string, error) {
		if calls.Add(1) == 1 {
			fmt.Fprintln(output, "output while running")
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return "error", ctx.Err()
			}
		}
		return "success", nil
	}
	j := admit(t, s, strings.Repeat("a", 40))
	admit(t, s, strings.Repeat("b", 40))
	stop := startDispatcher(t, s)
	<-started
	if calls.Load() != 1 {
		t.Fatal("parallelism limit exceeded")
	}
	w := httptest.NewRecorder()
	routes(s).ServeHTTP(w, httptest.NewRequest("GET", "/runs/"+j.ID+"/log", nil))
	if !strings.Contains(w.Body.String(), "output while running") {
		t.Fatal("live log missing")
	}
	close(release)
	eventually(t, func() bool { return calls.Load() == 2 })
	stop()
}

func TestRepositoryCannotBeRetargetedWithPendingStatuses(t *testing.T) {
	s := testServer(t)
	admit(t, s, strings.Repeat("a", 40))
	r := s.config.Repositories["test"]
	r.Name = "owner/other"
	s.config.Repositories["test"] = r
	if _, err := newServer(s.config, s.passcode, s.store); err == nil {
		t.Fatal("pending status could target a different repository")
	}
}

func TestWorkspaceCannotMoveBeforeCleanup(t *testing.T) {
	s := testServer(t)
	admit(t, s, strings.Repeat("a", 40))
	s.config.WorkRoot = filepath.Join(s.config.Root, "other-work")
	if _, err := newServer(s.config, s.passcode, s.store); err == nil {
		t.Fatal("lost track of original workspace")
	}
}

func storedJob(t *testing.T, s *server, id string) job {
	t.Helper()
	j, err := s.store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func startDispatcher(t *testing.T, s *server) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx = ctx
	done := make(chan struct{})
	go func() { defer close(done); s.dispatchJobs(ctx) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

// Inject a failed database write while keeping all other operations on SQLite.
type failingFinishStore struct {
	jobStore
	blocked   atomic.Bool
	attempted chan struct{}
	once      sync.Once
}

func (s *failingFinishStore) Finish(ctx context.Context, id, state, message string) error {
	if s.blocked.Load() {
		s.once.Do(func() { close(s.attempted) })
		return errors.New("database write temporarily unavailable")
	}
	return s.jobStore.Finish(ctx, id, state, message)
}

func TestResultWriteFailureRetainsDatabaseSlot(t *testing.T) {
	s := testServer(t)
	failure := &failingFinishStore{jobStore: s.store, attempted: make(chan struct{})}
	failure.blocked.Store(true)
	s.store = failure
	var executions atomic.Int32
	s.execute = func(context.Context, config, job, io.Writer) (string, error) {
		executions.Add(1)
		return "success", nil
	}
	first := admit(t, s, strings.Repeat("a", 40))
	second := admit(t, s, strings.Repeat("b", 40))
	stop := startDispatcher(t, s)
	defer stop()
	<-failure.attempted
	if j := storedJob(t, s, first.ID); j.State != "running" {
		t.Fatalf("failed write released job: %+v", j)
	}
	peer, err := openSQLiteStore(filepath.Join(s.config.Root, "jobs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if next, err := peer.Claim(context.Background(), 1); err != nil || next != nil {
		t.Fatalf("failed write freed a slot: %+v %v", next, err)
	}
	if executions.Load() != 1 {
		t.Fatal("dispatcher exceeded database limit")
	}
	failure.blocked.Store(false)
	eventually(t, func() bool {
		return storedJob(t, s, first.ID).State == "success" && storedJob(t, s, second.ID).State == "success"
	})
	if executions.Load() != 2 {
		t.Fatal("job was lost or executed twice")
	}
}
