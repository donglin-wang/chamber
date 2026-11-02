package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

var (
	errJobNotFound      = errors.New("job not found")
	errQueueFull        = errors.New("CI queue is full")
	errJobStateConflict = errors.New("job state conflict")
)

type job struct {
	ID             string    `json:"id"`
	Repository     string    `json:"repository"`
	RepositoryName string    `json:"repository_name"`
	Workspace      string    `json:"workspace"`
	SHA            string    `json:"sha"`
	Ref            string    `json:"ref"`
	State          string    `json:"state"`
	Message        string    `json:"message"`
	CreatedAt      time.Time `json:"created_at"`
	FinishedAt     time.Time `json:"finished_at,omitzero"`
	Reported       bool      `json:"reported"`
	Cleaned        bool      `json:"cleaned"`
	Version        int64     `json:"version"`
}

// jobStore owns durable jobs and the queue limits. Implementations must make
// Admit and Claim atomic, even when called through different store connections.
// The caller owns Close and must stop all users of the store before closing it.
type jobStore interface {
	// Admit returns an existing job on redelivery (created=false), even if full.
	Admit(ctx context.Context, j job, maxQueued int) (saved job, created bool, err error)
	Get(ctx context.Context, id string) (job, error)
	// Claim changes the oldest queued job to running only below maxParallel.
	// A nil job means the queue is empty or all execution slots are occupied.
	Claim(ctx context.Context, maxParallel int) (*job, error)
	Finish(ctx context.Context, id, state, message string) error
	// Unsettled lists jobs still needing status delivery or workspace cleanup.
	Unsettled(ctx context.Context) ([]job, error)
	// Acknowledge must ignore a stale version, so an old pending status cannot
	// acknowledge a terminal result that was saved while the request was in flight.
	Acknowledge(ctx context.Context, id string, version int64) error
	MarkCleaned(ctx context.Context, id string) error
	RunningCount(ctx context.Context) (int, error)
	CancelQueued(ctx context.Context) error
	// Call only when the old executor has stopped. Queued jobs are durable work,
	// not dropped executions; they remain queued across an update or crash.
	RecoverInterrupted(ctx context.Context) error
	Close() error
}

func jobID(repository, sha string) string {
	sum := sha256.Sum256([]byte(repository + "\x00" + sha))
	return hex.EncodeToString(sum[:16])
}

func (j job) terminal() bool {
	return j.State == "success" || j.State == "failure" || j.State == "error"
}
