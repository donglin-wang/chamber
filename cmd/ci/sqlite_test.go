package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func queuedJob(number int) job {
	sha := fmt.Sprintf("%040x", number)
	return job{ID: jobID("test", sha), Repository: "test", RepositoryName: "owner/repo", Workspace: "/tmp/test-work/" + sha,
		SHA: sha, Ref: "refs/heads/main", State: "queued", Message: "queued", CreatedAt: time.Unix(1700000000, int64(number)).UTC()}
}

func seedSettledJob(t *testing.T, root string) job {
	t.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := updatePasscode(root); err != nil {
		t.Fatal(err)
	}
	store, err := openSQLiteStore(filepath.Join(root, "jobs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	j := queuedJob(42)
	j.Workspace = jobWorkspace(config{Root: root}, j.ID)
	if _, _, err := store.Admit(ctx, j, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, j.ID, "success", "passed"); err != nil {
		t.Fatal(err)
	}
	finished, err := store.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Acknowledge(ctx, j.ID, finished.Version); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCleaned(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	return finished
}

// Another implementation can run this same contract without depending on SQL.
func testJobStoreContract(t *testing.T, open func(string) (jobStore, error)) {
	t.Helper()
	ctx := context.Background()
	newStore := func(t *testing.T) (jobStore, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "jobs.sqlite")
		store, err := open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		return store, path
	}
	t.Run("admission and execution", func(t *testing.T) {
		store, _ := newStore(t)
		first := queuedJob(1)
		saved, created, err := store.Admit(ctx, first, 1)
		if err != nil || !created || saved.Version != 1 || !saved.CreatedAt.Equal(first.CreatedAt) {
			t.Fatalf("admit: %+v %t %v", saved, created, err)
		}
		duplicate, created, err := store.Admit(ctx, first, 1)
		if err != nil || created || duplicate.ID != saved.ID {
			t.Fatalf("full-queue redelivery: %+v %t %v", duplicate, created, err)
		}
		second := queuedJob(2)
		if _, _, err := store.Admit(ctx, second, 1); !errors.Is(err, errQueueFull) {
			t.Fatalf("queue limit: %v", err)
		}
		if err := store.Finish(ctx, first.ID, "success", "passed"); !errors.Is(err, errJobStateConflict) {
			t.Fatalf("finished an unclaimed job: %v", err)
		}
		claimed, err := store.Claim(ctx, 1)
		if err != nil || claimed == nil || claimed.ID != first.ID || claimed.State != "running" || claimed.Version != 2 {
			t.Fatalf("claim: %+v %v", claimed, err)
		}
		if _, _, err := store.Admit(ctx, second, 1); err != nil {
			t.Fatal(err)
		}
		if j, err := store.Claim(ctx, 1); err != nil || j != nil {
			t.Fatalf("exceeded running limit: %+v %v", j, err)
		}
		if err := store.Finish(ctx, first.ID, "success", "passed"); err != nil {
			t.Fatal(err)
		}
		if err := store.Finish(ctx, first.ID, "success", "passed"); err != nil {
			t.Fatalf("finish retry: %v", err)
		}
		if err := store.Finish(ctx, first.ID, "failure", "failed"); !errors.Is(err, errJobStateConflict) {
			t.Fatalf("overwrote terminal outcome: %v", err)
		}
		next, err := store.Claim(ctx, 1)
		if err != nil || next == nil || next.ID != second.ID {
			t.Fatalf("did not release slot: %+v %v", next, err)
		}
		if _, err := store.Get(ctx, "missing"); !errors.Is(err, errJobNotFound) {
			t.Fatal(err)
		}
	})
	t.Run("versioned status acknowledgement", func(t *testing.T) {
		store, _ := newStore(t)
		j, _, err := store.Admit(ctx, queuedJob(1), 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Claim(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if err := store.Finish(ctx, j.ID, "success", "passed"); err != nil {
			t.Fatal(err)
		}
		if err := store.Acknowledge(ctx, j.ID, j.Version); err != nil {
			t.Fatal(err)
		}
		finished, err := store.Get(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if finished.Reported || finished.Version != 3 || finished.FinishedAt.IsZero() {
			t.Fatalf("stale acknowledgement: %+v", finished)
		}
		if err := store.Acknowledge(ctx, j.ID, finished.Version); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkCleaned(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
		unsettled, err := store.Unsettled(ctx)
		if err != nil || len(unsettled) != 0 {
			t.Fatalf("settled job still pending: %+v %v", unsettled, err)
		}
	})
	t.Run("interrupted jobs and durable retries", func(t *testing.T) {
		store, path := newStore(t)
		first, _, err := store.Admit(ctx, queuedJob(1), 2)
		if err != nil {
			t.Fatal(err)
		}
		second, _, err := store.Admit(ctx, queuedJob(2), 2)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.Claim(ctx, 1)
		if err != nil || claimed == nil {
			t.Fatalf("claim: %v", err)
		}
		if err := store.Acknowledge(ctx, claimed.ID, claimed.Version); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		if count, err := pendingCount(ctx, reopened); err != nil || count != 2 {
			t.Fatalf("lost persisted jobs: %d %v", count, err)
		}
		if err := reopened.RecoverInterrupted(ctx); err != nil {
			t.Fatal(err)
		}
		recovered, err := reopened.Get(ctx, first.ID)
		if err != nil || recovered.State != "error" || recovered.Reported || recovered.Cleaned || recovered.FinishedAt.IsZero() {
			t.Fatalf("bad recovery: %+v %v", recovered, err)
		}
		queued, err := reopened.Get(ctx, second.ID)
		if err != nil || queued.State != "queued" {
			t.Fatalf("lost queued job: %+v %v", queued, err)
		}
		if count, err := pendingCount(ctx, reopened); err != nil || count != 1 {
			t.Fatalf("wrong remaining work: %d %v", count, err)
		}
		if err := reopened.CancelQueued(ctx); err != nil {
			t.Fatal(err)
		}
		if count, err := pendingCount(ctx, reopened); err != nil || count != 0 {
			t.Fatalf("shutdown retained work: %d %v", count, err)
		}

	})
	t.Run("invalid jobs", func(t *testing.T) {
		store, _ := newStore(t)
		invalid := queuedJob(1)
		invalid.ID = "../../outside"
		if _, _, err := store.Admit(ctx, invalid, 1); err == nil {
			t.Fatal("unsafe ID accepted")
		}
		if _, err := store.Claim(ctx, 0); err == nil {
			t.Fatal("zero execution limit accepted")
		}
	})
}

func TestSQLiteStoreContract(t *testing.T) {
	testJobStoreContract(t, func(path string) (jobStore, error) { return openSQLiteStore(path) })
}

func sharedSQLiteStores(t *testing.T) []*sqliteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.sqlite")
	var stores []*sqliteStore
	for i := 0; i < 8; i++ {
		store, err := openSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		stores = append(stores, store)
		t.Cleanup(func() { store.Close() })
	}
	return stores
}

func TestSQLiteClaimsGateAcrossConnections(t *testing.T) {
	stores := sharedSQLiteStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 1; i <= 12; i++ {
		if _, _, err := stores[0].Admit(ctx, queuedJob(i), 12); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan *job, 32)
	errs := make(chan error, 32)
	var calls sync.WaitGroup
	for i := 0; i < 32; i++ {
		calls.Add(1)
		go func(i int) {
			defer calls.Done()
			<-start
			j, err := stores[i%len(stores)].Claim(ctx, 3)
			if err != nil {
				errs <- err
			}
			if j != nil {
				results <- j
			}
		}(i)
	}
	close(start)
	calls.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	claimed := map[string]bool{}
	for j := range results {
		if claimed[j.ID] {
			t.Fatal("job claimed twice")
		}
		claimed[j.ID] = true
	}
	if len(claimed) != 3 {
		t.Fatalf("32 calls through 8 connections claimed %d jobs; want 3", len(claimed))
	}
	for i := 1; i <= 3; i++ {
		if !claimed[queuedJob(i).ID] {
			t.Fatal("did not claim oldest queued jobs first")
		}
	}
	jobs, err := stores[0].Unsettled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	running := 0
	for _, j := range jobs {
		if j.State == "running" {
			running++
		}
	}
	if running != 3 {
		t.Fatalf("database has %d running jobs", running)
	}
	if err := stores[1].Finish(ctx, queuedJob(1).ID, "success", "passed"); err != nil {
		t.Fatal(err)
	}
	next, err := stores[2].Claim(ctx, 3)
	if err != nil || next == nil || next.ID != queuedJob(4).ID {
		t.Fatalf("slot release: %+v %v", next, err)
	}
}

func TestSQLiteQueueLimitAcrossConnections(t *testing.T) {
	stores := sharedSQLiteStores(t)
	ctx := context.Background()
	start := make(chan struct{})
	accepted := make(chan job, 32)
	errs := make(chan error, 32)
	var calls sync.WaitGroup
	for i := 1; i <= 32; i++ {
		calls.Add(1)
		go func(i int) {
			defer calls.Done()
			<-start
			j, created, err := stores[i%len(stores)].Admit(ctx, queuedJob(i), 4)
			if err != nil && !errors.Is(err, errQueueFull) {
				errs <- err
			}
			if created {
				accepted <- j
			}
		}(i)
	}
	close(start)
	calls.Wait()
	close(accepted)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var jobs []job
	for j := range accepted {
		jobs = append(jobs, j)
	}
	if len(jobs) != 4 {
		t.Fatalf("accepted %d queued jobs; want 4", len(jobs))
	}
	for _, store := range stores {
		if count, err := pendingCount(ctx, store); err != nil || count != 4 {
			t.Fatalf("queue count: %d %v", count, err)
		}
		if _, created, err := store.Admit(ctx, jobs[0], 4); err != nil || created {
			t.Fatalf("redelivery into full queue: %t %v", created, err)
		}
	}
}

func TestSQLiteFailedFinishKeepsSlotOccupied(t *testing.T) {
	stores := sharedSQLiteStores(t)
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		if _, _, err := stores[0].Admit(ctx, queuedJob(i), 2); err != nil {
			t.Fatal(err)
		}
	}
	first, err := stores[0].Claim(ctx, 1)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	if _, err := stores[0].db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	if err := stores[0].Finish(ctx, first.ID, "success", "passed"); err == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	if next, err := stores[1].Claim(ctx, 1); err != nil || next != nil {
		t.Fatalf("failed finish released slot: %+v %v", next, err)
	}
	if _, err := stores[0].db.Exec(`PRAGMA query_only=OFF`); err != nil {
		t.Fatal(err)
	}
	if err := stores[0].Finish(ctx, first.ID, "success", "passed"); err != nil {
		t.Fatal(err)
	}
	if next, err := stores[1].Claim(ctx, 1); err != nil || next == nil {
		t.Fatalf("durable finish did not release slot: %+v %v", next, err)
	}
}

func pendingCount(ctx context.Context, store jobStore) (int, error) {
	jobs, err := store.Unsettled(ctx)
	count := 0
	for _, j := range jobs {
		if !j.terminal() {
			count++
		}
	}
	return count, err
}
