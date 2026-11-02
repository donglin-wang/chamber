package main

import (
	"context"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFailedExecRestoresBinaryAndDescriptorFlags(t *testing.T) {
	root := t.TempDir()
	executable, candidate := filepath.Join(root, "ci"), filepath.Join(root, "candidate")
	for path, data := range map[string]string{executable: "old", candidate: "new"} {
		if err := os.WriteFile(path, []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	socket, err := os.CreateTemp(root, "socket")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	lock, err := os.CreateTemp(root, "lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	err = replaceProcess(candidate, executable, "config", socket, lock, func(path string, args, env []string) error {
		data, _ := os.ReadFile(path)
		if string(data) != "new" {
			t.Error("candidate not installed")
		}
		for _, file := range []*os.File{socket, lock} {
			flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
			if err != nil || flags&unix.FD_CLOEXEC != 0 {
				t.Error("descriptor cannot survive exec", err)
			}
		}
		return syscall.ENOEXEC
	})
	if !errors.Is(err, syscall.ENOEXEC) {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(executable)
	if string(data) != "old" {
		t.Fatal("previous binary not restored")
	}
	for _, file := range []*os.File{socket, lock} {
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Error("descriptor would leak to subprocess", err)
		}
	}
}

func TestUpdateDrainsRunningJobsAndKeepsAcceptingQueuedJobs(t *testing.T) {
	s := testServer(t)
	s.config.MaxQueued = 3
	started := make(chan struct{})
	release := make(chan struct{})
	s.execute = func(context.Context, config, job, io.Writer) (string, error) {
		close(started)
		<-release
		return "success", nil
	}
	s.buildUpdate = func(context.Context) (string, error) { return "candidate", nil }
	admit(t, s, strings.Repeat("a", 40))
	admit(t, s, strings.Repeat("b", 40))
	stop := startDispatcher(t, s)
	defer stop()
	<-started
	r := httptest.NewRequest("POST", "/update", nil)
	r.Header.Set("Authorization", "Bearer "+s.passcode)
	w := httptest.NewRecorder()
	routes(s).ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatalf("update: %d", w.Code)
	}
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.draining })
	select {
	case <-s.updateReady:
		t.Fatal("update skipped accepted jobs")
	default:
	}
	w = httptest.NewRecorder()
	routes(s).ServeHTTP(w, signedRequest(pushBody(strings.Repeat("c", 40)), "push"))
	if w.Code != 202 {
		t.Fatalf("rejected durable queued work while draining: %d", w.Code)
	}
	close(release)
	select {
	case <-s.updateReady:
	case <-time.After(time.Second):
		t.Fatal("update did not finish draining")
	}
	s.tasks.Wait()
	if n, err := pendingCount(context.Background(), s.store); err != nil || n != 2 {
		t.Fatalf("queued jobs did not transfer: %d %v", n, err)
	}
}

func TestShutdownFinishesDrainingAnUpdate(t *testing.T) {
	s := testServer(t)
	s.config.Listen = "127.0.0.1:0"
	started := make(chan struct{})
	s.execute = func(ctx context.Context, _ config, _ job, _ io.Writer) (string, error) {
		close(started)
		<-ctx.Done()
		return "error", ctx.Err()
	}
	candidate := filepath.Join(t.TempDir(), "candidate")
	s.buildUpdate = func(context.Context) (string, error) { return candidate, nil }
	first := admit(t, s, strings.Repeat("a", 40))
	second := admit(t, s, strings.Repeat("b", 40))
	shutdown, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, err := s.serve(shutdown, listener); done <- err }()
	<-started
	r := httptest.NewRequest("POST", "/update", nil)
	r.Header.Set("Authorization", "Bearer "+s.passcode)
	routes(s).ServeHTTP(httptest.NewRecorder(), r)
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.draining })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if j := storedJob(t, s, id); j.State != "error" {
			t.Fatalf("shutdown left job %s in %s", id, j.State)
		}
	}
	select {
	case <-s.updateReady:
		t.Fatal("activated update after shutdown")
	default:
	}
}

func TestShutdownHasDeadlineAndAttemptsFinalStatus(t *testing.T) {
	s := testServer(t)
	reported := make(chan string, 8)
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		reported <- body["state"]
		w.WriteHeader(201)
	}))
	defer github.Close()
	repo := s.config.Repositories["test"]
	repo.APIURL = github.URL
	s.config.Repositories["test"] = repo
	j := admit(t, s, strings.Repeat("a", 40))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.stop(ctx, &http.Server{}, func() {}, false); err != nil {
		t.Fatal(err)
	}
	if saved := storedJob(t, s, j.ID); saved.State != "error" || !saved.Reported {
		t.Fatalf("final status not delivered: %+v", saved)
	}
	select {
	case state := <-reported:
		if state != "error" {
			t.Fatal(state)
		}
	default:
		t.Fatal("no final request")
	}
	// An uncooperative task must not make shutdown wait forever.
	s.tasks.Add(1)
	short, done := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer done()
	if err := s.stop(short, &http.Server{}, func() {}, false); !errors.Is(err, errShutdownTimeout) {
		t.Fatal(err)
	}
	s.tasks.Done()
}
