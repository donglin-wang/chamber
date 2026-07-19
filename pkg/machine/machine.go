package machine

import (
	"context"
	"io"
)

const (
	StatusUnknown Status = "unknown"
	StatusMissing Status = "missing"
	StatusStopped Status = "stopped"
	StatusRunning Status = "running"
	StatusBroken  Status = "broken"
)

type Status string

type Spec struct {
	OS          string
	Arch        string
	CPUs        int
	MemoryBytes int64
	DiskBytes   int64
	Mounts      []Mount
	SetupScript string
}

type Mount struct {
	Source   string
	Target   string
	Writable bool
}

type Descriptor struct {
	Name        string
	Status      Status
	Provider    string
	Directory   string
	OS          string
	Arch        string
	CPUs        int
	MemoryBytes int64
	DiskBytes   int64
}

type RunRequest struct {
	Args    []string
	Workdir string
	Env     []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

type RunResult struct {
	ExitCode int
}

type Machine interface {
	Descriptor() Descriptor
	Run(context.Context, RunRequest) (RunResult, error)
	Stop(context.Context) error
	Delete(context.Context) error
}
