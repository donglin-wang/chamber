package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

func TestStartupCheckDoesNotChangeJobsOrCreateCredentials(t *testing.T) {
	s := testServer(t)
	s.config.WorkRoot = filepath.Join(s.config.Root, "work")
	if err := hostfs.CreatePrivateDirectory(s.config.WorkRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := updatePasscode(s.config.Root); err == nil {
		t.Fatal("created a passcode for existing state")
	}
	// A valid installation always creates its secret before its database.
	if err := writeAtomic(filepath.Join(s.config.Root, "update.passcode"), []byte(strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := updatePasscode(s.config.Root); err != nil {
		t.Fatal(err)
	}
	j := admit(t, s, strings.Repeat("a", 40))
	if _, err := s.store.Claim(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := checkStartup(context.Background(), s.config); err != nil {
		t.Fatal(err)
	}
	if got := storedJob(t, s, j.ID); got.State != "running" || got.Version != 2 {
		t.Fatalf("check changed running job: %+v", got)
	}
	if err := os.Remove(filepath.Join(s.config.Root, "update.passcode")); err != nil {
		t.Fatal(err)
	}
	if err := checkStartup(context.Background(), s.config); err == nil {
		t.Fatal("check accepted missing credential")
	}
	if _, err := os.Stat(filepath.Join(s.config.Root, "update.passcode")); !os.IsNotExist(err) {
		t.Fatal("check generated credential", err)
	}
}

func TestCandidateCheckRejectsFailureAndInvalidAddress(t *testing.T) {
	for _, body := range []string{"exit 1", "echo nonsense", "echo 192.0.2.1:80"} {
		path := filepath.Join(t.TempDir(), "candidate")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := checkCandidate(context.Background(), path, "unused"); err == nil {
			t.Fatal("invalid candidate accepted")
		}
	}
}

func TestCandidateMustAnswerHealthWithinDeadline(t *testing.T) {
	for _, status := range []int{200, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			expected := strings.Repeat("c", 40)
			probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/health" {
					t.Error("unexpected health path", r.URL.Path)
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, expected)
			}))
			defer probe.Close()
			path := filepath.Join(t.TempDir(), "candidate")
			script := "#!/bin/sh\necho " + strings.TrimPrefix(probe.URL, "http://") + "\nsleep 60\n"
			if err := os.WriteFile(path, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			rev, err := checkCandidate(context.Background(), path, "unused")
			if status == 200 {
				if err != nil || rev != expected {
					t.Fatal(rev, err)
				}
			} else if err == nil {
				t.Fatal("unhealthy candidate accepted")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := checkCandidate(ctx, path, "unused"); err == nil {
		t.Fatal("accepted candidate that never became ready")
	}
	if time.Since(started) > time.Second {
		t.Fatal("readiness timeout was not bounded")
	}
}
