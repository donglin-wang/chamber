package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	chamberRuntimeShared "github.com/donglin-wang/chamber/pkg/runtime/shared"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

func TestResolveRootDefaultsOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()

	root, err := resolveRoot("", workspace)
	if err != nil {
		t.Fatalf("resolveRoot() error = %v", err)
	}
	if root == "" {
		t.Fatal("resolveRoot() = empty, want cache root")
	}
	if pathContains(workspace, root) {
		t.Fatalf("default root %q is inside workspace %q", root, workspace)
	}
	if filepath.Base(root) != "ci" || filepath.Base(filepath.Dir(root)) != "chamber" {
		t.Fatalf("default root = %q, want path ending in chamber/ci", root)
	}
}

func TestResolveRootRejectsWorkspaceRoot(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".chamber-ci")

	if _, err := resolveRoot(root, workspace); err == nil {
		t.Fatal("resolveRoot() error = nil, want workspace root rejection")
	}
}

func TestResolveRootAllowsExternalRoot(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(t.TempDir(), "chamber-ci")

	resolved, err := resolveRoot(root, workspace)
	if err != nil {
		t.Fatalf("resolveRoot() error = %v", err)
	}
	if resolved != root {
		t.Fatalf("resolveRoot() = %q, want %q", resolved, root)
	}
}

func TestWaitForContainerForceDeletesWhenContextExpires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	container := newBlockingContainer()

	done := make(chan waitOutcome, 1)
	go func() {
		result, err := waitForContainer(ctx, container)
		done <- waitOutcome{result: result, err: err}
	}()

	<-container.waitStarted
	cancel()

	outcome := <-done
	if !errors.Is(outcome.err, chamberErrors.ErrCanceled) {
		t.Fatalf("waitForContainer() error = %v, want canceled code", outcome.err)
	}
	if outcome.result.ExitCode != 137 {
		t.Fatalf("ExitCode = %d, want 137 from deleted container", outcome.result.ExitCode)
	}
	if !container.forceDeleted {
		t.Fatal("container was not force deleted")
	}
}

func TestLogResultUsesStructuredLogger(t *testing.T) {
	var buffer bytes.Buffer
	old := chamberLogging.Logger()
	chamberLogging.SetLogger(chamberLogging.NewJSONLogger(&buffer, slog.LevelInfo))
	t.Cleanup(func() {
		chamberLogging.SetLogger(old)
	})

	logResult(context.Background(), jobResult{
		name:     "pkg",
		exitCode: 0,
		stdout:   []byte("ok github.com/donglin-wang/chamber/pkg/image\n"),
	})

	entries := decodeLogEntries(t, buffer.Bytes())
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2; output = %s", len(entries), buffer.String())
	}
	if entries[0]["msg"] != "CI job output" {
		t.Fatalf("first msg = %v, want CI job output", entries[0]["msg"])
	}
	if entries[0]["job"] != "pkg" || entries[0]["stream"] != "stdout" {
		t.Fatalf("first entry job/stream = %v/%v, want pkg/stdout", entries[0]["job"], entries[0]["stream"])
	}
	if entries[0]["output"] != "ok github.com/donglin-wang/chamber/pkg/image\n" {
		t.Fatalf("first entry output = %v", entries[0]["output"])
	}
	if entries[1]["msg"] != "CI job passed" || entries[1]["job"] != "pkg" {
		t.Fatalf("second entry = %#v, want passed pkg event", entries[1])
	}
}

type blockingContainer struct {
	waitStarted chan struct{}
	deleted     chan struct{}
	deleteOnce  sync.Once

	forceDeleted bool
}

func newBlockingContainer() *blockingContainer {
	return &blockingContainer{
		waitStarted: make(chan struct{}),
		deleted:     make(chan struct{}),
	}
}

func (c *blockingContainer) ID() string { return "blocking-container" }

func (c *blockingContainer) StdoutPath() string { return "" }

func (c *blockingContainer) StderrPath() string { return "" }

func (c *blockingContainer) Wait() (chamberRuntimeShared.ContainerResult, error) {
	close(c.waitStarted)
	<-c.deleted
	return chamberRuntimeShared.ContainerResult{ExitCode: 137}, nil
}

func (c *blockingContainer) State(ctx context.Context) (chamberRuntimeShared.ContainerState, error) {
	return chamberRuntimeShared.ContainerState{ContainerID: c.ID()}, ctx.Err()
}

func (c *blockingContainer) Signal(ctx context.Context, signal os.Signal) error {
	return ctx.Err()
}

func (c *blockingContainer) Delete(ctx context.Context, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.forceDeleted = force
	c.deleteOnce.Do(func() {
		close(c.deleted)
	})
	return nil
}

func (c *blockingContainer) ReadLog(chamberRuntimeShared.LogStream) ([]byte, error) {
	return nil, nil
}

func (c *blockingContainer) DeleteLog(chamberRuntimeShared.LogStream) error {
	return nil
}

func decodeLogEntries(t *testing.T, data []byte) []map[string]any {
	t.Helper()

	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode log entry %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}
