package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	"github.com/donglin-wang/chamber/daemon/metadata/memory"
)

func TestReconcileRunningOperationsFailsUnprovenWorkAndKeepsOwnedWork(t *testing.T) {
	store := memory.NewMemoryStore()
	now := time.Now().UTC()
	operations := []metadata.Operation{
		{ID: "pull-completed", Kind: metadata.PullOperation, State: metadata.OperationRunning, ResourceID: "docker.io/library/alpine:latest", StartedAt: now, UpdatedAt: now},
		{ID: "start-orphan", Kind: metadata.StartOperation, State: metadata.OperationRunning, ResourceID: "missing-container", StartedAt: now, UpdatedAt: now},
		{ID: "run-owned", Kind: metadata.RunOperation, State: metadata.OperationRunning, ResourceID: "running-container", StartedAt: now, UpdatedAt: now},
		{ID: "create-finished", Kind: metadata.CreateOperation, State: metadata.OperationRunning, ResourceID: "created-container", StartedAt: now, UpdatedAt: now},
		{ID: "run-failed", Kind: metadata.RunOperation, State: metadata.OperationRunning, ResourceID: "failed-container", StartedAt: now, UpdatedAt: now},
		{ID: "stop-completed", Kind: metadata.StopOperation, State: metadata.OperationRunning, ResourceID: "exited-container", StartedAt: now, UpdatedAt: now},
	}
	for _, operation := range operations {
		if err := store.CreateOperation(context.Background(), operation); err != nil {
			t.Fatalf("CreateOperation(%q) error = %v", operation.ID, err)
		}
	}
	// Existing resource state is deliberately present: it does not prove these
	// interrupted pull and stop operations themselves completed.
	if err := store.PutImage(context.Background(), metadata.Image{Reference: operations[0].ResourceID, Digest: "sha256:image", PulledAt: now, LastUsedAt: now}); err != nil {
		t.Fatalf("PutImage() error = %v", err)
	}
	for _, container := range []metadata.Container{
		{ID: "running-container", OperationID: "run-owned", State: metadata.ContainerRunning, CreatedAt: now, UpdatedAt: now},
		{ID: "created-container", OperationID: "create-finished", State: metadata.ContainerCreated, CreatedAt: now, UpdatedAt: now},
		{ID: "failed-container", OperationID: "run-failed", State: metadata.ContainerFailed, ErrorCode: "runtime_wait_failed", CreatedAt: now, UpdatedAt: now},
		{ID: "exited-container", OperationID: "run-exited", State: metadata.ContainerExited, CreatedAt: now, UpdatedAt: now},
	} {
		if err := store.CreateContainer(context.Background(), container); err != nil {
			t.Fatalf("CreateContainer(%q) error = %v", container.ID, err)
		}
	}

	if err := reconcileRunningOperations(context.Background(), store); err != nil {
		t.Fatalf("reconcileRunningOperations() error = %v", err)
	}
	want := map[string]metadata.OperationState{
		"pull-completed":  metadata.OperationFailed,
		"start-orphan":    metadata.OperationFailed,
		"run-owned":       metadata.OperationRunning,
		"create-finished": metadata.OperationSucceeded,
		"run-failed":      metadata.OperationFailed,
		"stop-completed":  metadata.OperationFailed,
	}
	for id, state := range want {
		operation, err := store.GetOperation(context.Background(), id)
		if err != nil {
			t.Fatalf("GetOperation(%q) error = %v", id, err)
		}
		if operation.State != state {
			t.Fatalf("operation %q state = %q, want %q", id, operation.State, state)
		}
	}
}

func TestListOperationsRouteReportsPendingCleanupCount(t *testing.T) {
	store := memory.NewMemoryStore()
	now := time.Now().UTC()
	operation := metadata.Operation{ID: "operation-list", Kind: metadata.PullOperation, State: metadata.OperationSucceeded, ResourceID: "image", StartedAt: now, UpdatedAt: now}
	if err := store.CreateOperation(context.Background(), operation); err != nil {
		t.Fatalf("CreateOperation() error = %v", err)
	}
	if err := store.CreateCleanup(context.Background(), metadata.Cleanup{ContainerID: "container-list", OperationID: "cleanup-list", Kind: metadata.CleanupOperation, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateCleanup() error = %v", err)
	}

	mux := newServer()
	registerOperationRoutes(mux, store)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/operations", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response listOperationsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Operations) != 1 || response.PendingCleanups != 1 {
		t.Fatalf("response = %#v", response)
	}
}
