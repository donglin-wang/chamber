package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckoutRequiresExactCommitAndRootDockerfile(t *testing.T) {
	for _, mode := range []string{"missing Dockerfile", "symlink Dockerfile", "moved branch"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t)
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "README"), []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "symlink Dockerfile" {
				if err := os.Symlink("README", filepath.Join(source, "Dockerfile.chamber")); err != nil {
					t.Fatal(err)
				}
			}
			sha := commitFixture(t, source)
			if mode == "moved branch" {
				sha = strings.Repeat("a", 40)
			}
			r := s.config.Repositories["test"]
			r.CloneURL = source
			s.config.Repositories["test"] = r
			j := job{ID: jobID("test", sha), Repository: "test", SHA: sha, Ref: "refs/heads/main"}
			defer func() {
				if err := cleanupJob(context.Background(), s.config, j); err != nil {
					t.Error(err)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			state, err := executeJob(ctx, s.config, j, io.Discard)
			if err == nil {
				t.Fatal("invalid checkout executed")
			}
			if mode == "moved branch" {
				if state != "error" || !strings.Contains(err.Error(), "does not match") {
					t.Fatalf("%s %v", state, err)
				}
			} else if state != "failure" {
				t.Fatalf("%s %v", state, err)
			}
		})
	}
}

func TestCleanupJobSkipsProvisionerHelperDirectories(t *testing.T) {
	s := testServer(t)
	j := job{ID: "helper-cleanup"}
	work := jobWorkspace(s.config, j.ID)
	for _, relative := range []string{"bundles/.bin", "tmp/bundles/.bin"} {
		helper := filepath.Join(work, "run", "runtime", relative, "umoci")
		if err := os.MkdirAll(filepath.Dir(helper), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(helper, []byte("managed helper"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupJob(context.Background(), s.config, j); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("workspace remains: %v", err)
	}
}
