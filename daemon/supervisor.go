package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

const (
	supervisorFileVersion = 1

	supervisorReconcileInterval = time.Second
	supervisorCreatingTimeout   = 30 * time.Second
)

var supervisorProcessAlive = processAlive

type supervisorPhase string

const (
	supervisorPrepared  supervisorPhase = "prepared"
	supervisorStarted   supervisorPhase = "started"
	supervisorCompleted supervisorPhase = "completed"
)

type supervisorFile struct {
	Version int             `json:"version"`
	Phase   supervisorPhase `json:"phase"`

	OperationID string `json:"operation_id"`
	ContainerID string `json:"container_id"`

	Runtime chamberRuntime.Config           `json:"runtime"`
	Bundle  chamberBundle.ProvisionedBundle `json:"bundle"`

	StdoutPath string `json:"stdout_path"`
	StderrPath string `json:"stderr_path"`

	RuntimeName string                          `json:"runtime_name,omitempty"`
	StartedAt   *time.Time                      `json:"started_at,omitempty"`
	Result      *chamberRuntime.ContainerResult `json:"result,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func runRuntimeSupervisor(ctx context.Context, args []string) error {
	var supervisorPath string

	fs := flag.NewFlagSet("chamberd runtime-supervisor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&supervisorPath, "supervisor", "", "supervisor file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if strings.TrimSpace(supervisorPath) == "" {
		return fmt.Errorf("%w: supervisor file path is required", chamberErrors.ErrInvalidRequest)
	}
	return runSupervisorFile(ctx, supervisorPath)
}

func runSupervisorFile(ctx context.Context, path string) error {
	state, err := readRequiredSupervisorFile(path)
	if err != nil {
		return err
	}

	failedResult := func(err error) chamberRuntime.ContainerResult {
		return chamberRuntime.ContainerResult{
			ContainerID: state.ContainerID,
			Status:      chamberRuntime.ContainerResultStatusStartFailed,
			ErrorCode:   chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeStartFailed),
			Error:       err.Error(),
			ExitedAt:    time.Now().UTC(),
		}
	}

	rt, err := chamberRuntimeFactory.NewRuntime(ctx, state.Runtime)
	if err != nil {
		return errors.Join(err, completeSupervisorFile(path, failedResult(err)))
	}

	stdout, err := openSupervisorLog(state.StdoutPath)
	if err != nil {
		return errors.Join(err, completeSupervisorFile(path, failedResult(err)))
	}
	defer stdout.Close()
	stderr, err := openSupervisorLog(state.StderrPath)
	if err != nil {
		return errors.Join(err, completeSupervisorFile(path, failedResult(err)))
	}
	defer stderr.Close()

	result, runErr := chamberRuntime.RunAndWait(context.Background(), rt, chamberRuntime.RunRequest{
		Bundle: state.Bundle,
		Stdout: []io.Writer{stdout},
		Stderr: []io.Writer{stderr},
	}, func(_ chamberRuntime.ContainerHandle) error {
		return markSupervisorStarted(path, rt.Descriptor().Name, time.Now().UTC())
	})
	return errors.Join(runErr, completeSupervisorFile(path, result))
}

type startSupervisorFunc func(ctx context.Context, supervisorPath string) (int, func() error, error)

func startSupervisorProcess(ctx context.Context, supervisorPath string) (int, func() error, error) {
	if ctx == nil {
		return 0, nil, fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, fmt.Errorf("%w: supervisor launch canceled before start: %w", chamberErrors.ErrCanceled, err)
	}
	executable, err := os.Executable()
	if err != nil {
		return 0, nil, fmt.Errorf("%w: resolve daemon executable: %w", chamberErrors.ErrRuntimeStartFailed, err)
	}
	command := exec.Command(executable, "runtime-supervisor", "--supervisor", supervisorPath)
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, nil, fmt.Errorf("%w: start runtime supervisor: %w", chamberErrors.ErrRuntimeStartFailed, err)
	}
	return command.Process.Pid, command.Wait, nil
}

func monitorSupervisor(ctx context.Context, store metadata.Store, operationID string, containerID string, supervisorPath string, waitSupervisor func() error) {
	if waitSupervisor == nil {
		return
	}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- waitSupervisor()
	}()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := reconcileSupervisorContainerID(ctx, store, containerID); err != nil {
				fmt.Fprintf(os.Stderr, "record supervisor state: operation=%s container=%s error=%v\n", operationID, containerID, err)
			}
		case waitErr := <-waitDone:
			if err := recordSupervisorExit(ctx, store, operationID, containerID, supervisorPath, waitErr); err != nil {
				fmt.Fprintf(os.Stderr, "record supervisor result: operation=%s container=%s error=%v\n", operationID, containerID, err)
			}
			return
		}
	}
}

func watchSupervisorContainers(ctx context.Context, store metadata.Store) {
	reconcileSupervisorContainers(ctx, store)

	ticker := time.NewTicker(supervisorReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileSupervisorContainers(ctx, store)
		}
	}
}

func reconcileSupervisorContainers(ctx context.Context, store metadata.Store) {
	containers, err := store.ListContainers(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile supervisor containers: error=%v\n", err)
		return
	}
	for _, container := range containers {
		if container.State != metadata.ContainerCreating &&
			container.State != metadata.ContainerStarting &&
			container.State != metadata.ContainerRunning {
			continue
		}
		if err := reconcileSupervisorContainer(ctx, store, container); err != nil {
			fmt.Fprintf(os.Stderr, "reconcile supervisor container: operation=%s container=%s error=%v\n", container.OperationID, container.ID, err)
		}
	}
}

func reconcileSupervisorContainerID(ctx context.Context, store metadata.Store, containerID string) error {
	container, err := store.GetContainer(ctx, containerID)
	if err != nil {
		return err
	}
	return reconcileSupervisorContainer(ctx, store, container)
}

func reconcileSupervisorContainer(ctx context.Context, store metadata.Store, container metadata.Container) error {
	if strings.TrimSpace(container.SupervisorPath) == "" {
		if container.State == metadata.ContainerCreating {
			return failContainerFromCurrent(ctx, store, container.OperationID, container.ID, chamberErrors.ErrRuntimeStartFailed, fmt.Errorf("creating container has no supervisor file"))
		}
		return nil
	}

	state, present, err := readSupervisorFile(container.SupervisorPath, container.OperationID, container.ID)
	if err != nil {
		return failContainerFromCurrent(ctx, store, container.OperationID, container.ID, chamberErrors.ErrRuntimeWaitFailed, err)
	}
	if !present {
		if container.State == metadata.ContainerCreating && time.Since(container.UpdatedAt) > supervisorCreatingTimeout {
			return failContainerFromCurrent(ctx, store, container.OperationID, container.ID, chamberErrors.ErrRuntimeStartFailed, fmt.Errorf("supervisor file was not created before recovery timeout"))
		}
		return nil
	}

	switch state.Phase {
	case supervisorPrepared:
		if container.State == metadata.ContainerCreating && time.Since(container.UpdatedAt) > supervisorCreatingTimeout {
			return failContainerFromCurrent(ctx, store, container.OperationID, container.ID, chamberErrors.ErrRuntimeStartFailed, fmt.Errorf("supervisor did not start before recovery timeout"))
		}
		return nil
	case supervisorStarted:
		if err := recordSupervisorStarted(ctx, store, container.ID); err != nil {
			return err
		}
		return failContainerIfSupervisorProcessExited(ctx, store, container)
	case supervisorCompleted:
		return applySupervisorResult(ctx, store, container.OperationID, container.ID, *state.Result)
	default:
		return failContainerFromCurrent(ctx, store, container.OperationID, container.ID, chamberErrors.ErrRuntimeWaitFailed, fmt.Errorf("unknown supervisor phase %q", state.Phase))
	}
}

func failContainerIfSupervisorProcessExited(ctx context.Context, store metadata.Store, container metadata.Container) error {
	alive, err := supervisorProcessAlive(container.SupervisorPID)
	if err != nil {
		return err
	}
	if alive {
		return nil
	}
	return failContainerFromCurrent(
		ctx,
		store,
		container.OperationID,
		container.ID,
		chamberErrors.ErrRuntimeWaitFailed,
		fmt.Errorf("supervisor process %d exited without completed result", container.SupervisorPID),
	)
}

func processAlive(pid int) (bool, error) {
	if pid <= 0 {
		return true, nil
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		if errors.Is(err, syscall.EPERM) {
			return true, nil
		}
		return false, fmt.Errorf("%w: inspect supervisor process %d: %w", chamberErrors.ErrRuntimeControlFailed, pid, err)
	}
	return true, nil
}

func recordSupervisorStarted(ctx context.Context, store metadata.Store, containerID string) error {
	container, err := store.GetContainer(ctx, containerID)
	if err != nil {
		return err
	}
	if container.State == metadata.ContainerRunning ||
		container.State == metadata.ContainerExited ||
		container.State == metadata.ContainerFailed {
		return nil
	}
	if container.State == metadata.ContainerCreating {
		_, err = store.TransitionContainer(ctx, containerID, metadata.ContainerCreating, metadata.ContainerUpdate{
			State: metadata.ContainerStarting,
			At:    time.Now().UTC(),
		})
		if err != nil && !errors.Is(err, chamberErrors.ErrStateConflict) {
			return err
		}
	}
	_, err = store.TransitionContainer(ctx, containerID, metadata.ContainerStarting, metadata.ContainerUpdate{
		State: metadata.ContainerRunning,
		At:    time.Now().UTC(),
	})
	return ignoreTerminalConflict(err)
}

func recordSupervisorExit(ctx context.Context, store metadata.Store, operationID string, containerID string, supervisorPath string, waitErr error) error {
	state, present, err := readSupervisorFile(supervisorPath, operationID, containerID)
	if err != nil {
		return failContainerFromCurrent(ctx, store, operationID, containerID, chamberErrors.ErrRuntimeWaitFailed, err)
	}
	if !present || state.Phase != supervisorCompleted {
		err := fmt.Errorf("%w: supervisor exited without completed result", chamberErrors.ErrRuntimeWaitFailed)
		if waitErr != nil {
			err = fmt.Errorf("%w: supervisor exited without completed result: %w", chamberErrors.ErrRuntimeWaitFailed, waitErr)
		}
		return failContainerFromCurrent(ctx, store, operationID, containerID, chamberErrors.ErrRuntimeWaitFailed, err)
	}
	return applySupervisorResult(ctx, store, operationID, containerID, *state.Result)
}

func applySupervisorResult(ctx context.Context, store metadata.Store, operationID string, containerID string, result chamberRuntime.ContainerResult) error {
	switch result.Status {
	case chamberRuntime.ContainerResultStatusExited:
		exitCode := 1
		if result.ExitCode != nil {
			exitCode = *result.ExitCode
		}
		code := chamberErrors.Code("")
		if exitCode != 0 {
			code = chamberErrors.ErrContainerExitNonzero
		}
		if err := exitContainerFromCurrent(ctx, store, operationID, containerID, exitCode, code, result.ExitedAt); err != nil {
			return err
		}
		if exitCode == 0 {
			_, err := store.SucceedOperation(ctx, operationID)
			return ignoreTerminalConflict(err)
		}
		_, err := store.FailOperation(ctx, operationID, code)
		return ignoreTerminalConflict(err)
	case chamberRuntime.ContainerResultStatusStartFailed:
		code := supervisorResultCode(result, chamberErrors.ErrRuntimeStartFailed)
		return failContainerFromCurrent(ctx, store, operationID, containerID, code, errors.New(result.Error))
	case chamberRuntime.ContainerResultStatusCanceled:
		code := supervisorResultCode(result, chamberErrors.ErrCanceled)
		if err := transitionContainerFailedFromCurrent(ctx, store, containerID, code); err != nil {
			return err
		}
		_, err := store.TransitionOperation(ctx, operationID, metadata.OperationRunning, metadata.OperationUpdate{
			State:     metadata.OperationAborted,
			At:        result.ExitedAt,
			ErrorCode: code,
		})
		return ignoreTerminalConflict(err)
	case chamberRuntime.ContainerResultStatusUnknown:
		code := supervisorResultCode(result, chamberErrors.ErrRuntimeWaitFailed)
		return failContainerFromCurrent(ctx, store, operationID, containerID, code, errors.New(result.Error))
	default:
		return failContainerFromCurrent(ctx, store, operationID, containerID, chamberErrors.ErrRuntimeWaitFailed, fmt.Errorf("unknown supervisor result status %q", result.Status))
	}
}

func exitContainerFromCurrent(ctx context.Context, store metadata.Store, operationID string, containerID string, exitCode int, code chamberErrors.Code, at time.Time) error {
	container, err := store.GetContainer(ctx, containerID)
	if err != nil {
		return err
	}
	switch container.State {
	case metadata.ContainerExited, metadata.ContainerFailed:
		return nil
	case metadata.ContainerCreating, metadata.ContainerStarting, metadata.ContainerRunning:
		_, err := store.TransitionContainer(ctx, containerID, container.State, metadata.ContainerUpdate{
			State:     metadata.ContainerExited,
			At:        at,
			ExitCode:  &exitCode,
			ErrorCode: code,
		})
		return err
	default:
		return failContainerFromCurrent(ctx, store, operationID, containerID, chamberErrors.ErrRuntimeWaitFailed, fmt.Errorf("cannot mark container exited from state %q", container.State))
	}
}

func failContainerFromCurrent(ctx context.Context, store metadata.Store, operationID string, containerID string, code chamberErrors.Code, err error) error {
	transitionErr := transitionContainerFailedFromCurrent(ctx, store, containerID, code)
	_, operationErr := store.FailOperation(ctx, operationID, code)
	return errors.Join(err, transitionErr, ignoreTerminalConflict(operationErr))
}

func transitionContainerFailedFromCurrent(ctx context.Context, store metadata.Store, containerID string, code chamberErrors.Code) error {
	container, getErr := store.GetContainer(ctx, containerID)
	if getErr != nil {
		return getErr
	}
	if container.State != metadata.ContainerFailed && container.State != metadata.ContainerExited {
		_, transitionErr := store.TransitionContainer(ctx, containerID, container.State, metadata.ContainerUpdate{
			State:     metadata.ContainerFailed,
			At:        time.Now().UTC(),
			ErrorCode: code,
		})
		return transitionErr
	}
	return nil
}

func supervisorResultCode(result chamberRuntime.ContainerResult, fallback chamberErrors.Code) chamberErrors.Code {
	if result.ErrorCode == "" {
		return fallback
	}
	return result.ErrorCode
}

func ignoreTerminalConflict(err error) error {
	if errors.Is(err, chamberErrors.ErrStateConflict) {
		return nil
	}
	return err
}

func openSupervisorLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("%w: create runtime log directory: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: open runtime log %q: %w", chamberErrors.ErrFilesystemFailed, path, err)
	}
	return file, nil
}

func writeSupervisorFile(path string, state supervisorFile) error {
	if err := validateSupervisorFile(state, state.OperationID, state.ContainerID); err != nil {
		return err
	}
	return writeSupervisorJSON(path, state)
}

func readRequiredSupervisorFile(path string) (supervisorFile, error) {
	state, present, err := readSupervisorFile(path, "", "")
	if err != nil {
		return supervisorFile{}, err
	}
	if !present {
		return supervisorFile{}, fmt.Errorf("%w: supervisor file %q not found", chamberErrors.ErrFilesystemFailed, path)
	}
	return state, nil
}

func readSupervisorFile(path string, operationID string, containerID string) (supervisorFile, bool, error) {
	state, present, err := readSupervisorJSON[supervisorFile](path)
	if err != nil || !present {
		return supervisorFile{}, present, err
	}
	if err := validateSupervisorFile(state, operationID, containerID); err != nil {
		return supervisorFile{}, true, err
	}
	return state, true, nil
}

func markSupervisorStarted(path string, runtimeName string, at time.Time) error {
	state, err := readRequiredSupervisorFile(path)
	if err != nil {
		return err
	}
	startedAt := at
	state.Phase = supervisorStarted
	state.RuntimeName = runtimeName
	state.StartedAt = &startedAt
	state.Result = nil
	state.UpdatedAt = at
	return writeSupervisorFile(path, state)
}

func completeSupervisorFile(path string, result chamberRuntime.ContainerResult) error {
	state, err := readRequiredSupervisorFile(path)
	if err != nil {
		return err
	}
	if result.ContainerID == "" {
		result.ContainerID = state.ContainerID
	}
	if result.StartedAt == nil && state.StartedAt != nil {
		result.StartedAt = state.StartedAt
	}
	now := time.Now().UTC()
	state.Phase = supervisorCompleted
	state.Result = &result
	state.UpdatedAt = now
	return writeSupervisorFile(path, state)
}

func validateSupervisorFile(state supervisorFile, operationID string, containerID string) error {
	if state.Version != supervisorFileVersion {
		return fmt.Errorf("%w: unsupported supervisor file version %d", chamberErrors.ErrInvalidRequest, state.Version)
	}
	if operationID != "" && state.OperationID != operationID {
		return fmt.Errorf("%w: supervisor operation id %q does not match %q", chamberErrors.ErrInvalidRequest, state.OperationID, operationID)
	}
	if containerID != "" && state.ContainerID != containerID {
		return fmt.Errorf("%w: supervisor container id %q does not match %q", chamberErrors.ErrInvalidRequest, state.ContainerID, containerID)
	}
	if strings.TrimSpace(state.OperationID) == "" {
		return fmt.Errorf("%w: supervisor operation id is required", chamberErrors.ErrInvalidRequest)
	}
	if strings.TrimSpace(state.ContainerID) == "" {
		return fmt.Errorf("%w: supervisor container id is required", chamberErrors.ErrInvalidRequest)
	}
	if state.Bundle.ContainerID != state.ContainerID {
		return fmt.Errorf("%w: supervisor container id %q does not match bundle container id %q", chamberErrors.ErrInvalidRequest, state.ContainerID, state.Bundle.ContainerID)
	}
	if strings.TrimSpace(state.Bundle.BundlePath) == "" {
		return fmt.Errorf("%w: supervisor bundle path is required", chamberErrors.ErrInvalidRequest)
	}
	if strings.TrimSpace(state.Runtime.Name) == "" {
		return fmt.Errorf("%w: supervisor runtime name is required", chamberErrors.ErrInvalidRequest)
	}
	if strings.TrimSpace(state.StdoutPath) == "" || strings.TrimSpace(state.StderrPath) == "" {
		return fmt.Errorf("%w: supervisor log paths are required", chamberErrors.ErrInvalidRequest)
	}
	if state.CreatedAt.IsZero() || state.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: supervisor timestamps are required", chamberErrors.ErrInvalidRequest)
	}

	switch state.Phase {
	case supervisorPrepared:
		if state.StartedAt != nil || state.RuntimeName != "" || state.Result != nil {
			return fmt.Errorf("%w: prepared supervisor file must not contain started or result evidence", chamberErrors.ErrInvalidRequest)
		}
	case supervisorStarted:
		if strings.TrimSpace(state.RuntimeName) == "" || state.StartedAt == nil || state.StartedAt.IsZero() {
			return fmt.Errorf("%w: started supervisor file requires runtime name and started time", chamberErrors.ErrInvalidRequest)
		}
		if state.Result != nil {
			return fmt.Errorf("%w: started supervisor file must not contain result evidence", chamberErrors.ErrInvalidRequest)
		}
	case supervisorCompleted:
		if state.Result == nil {
			return fmt.Errorf("%w: completed supervisor file requires result evidence", chamberErrors.ErrInvalidRequest)
		}
		if err := validateSupervisorResult(*state.Result, state.ContainerID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: unknown supervisor phase %q", chamberErrors.ErrInvalidRequest, state.Phase)
	}
	return nil
}

func validateSupervisorResult(result chamberRuntime.ContainerResult, containerID string) error {
	if result.ContainerID != containerID {
		return fmt.Errorf("%w: supervisor result container id %q does not match %q", chamberErrors.ErrInvalidRequest, result.ContainerID, containerID)
	}
	switch result.Status {
	case chamberRuntime.ContainerResultStatusExited,
		chamberRuntime.ContainerResultStatusStartFailed,
		chamberRuntime.ContainerResultStatusCanceled,
		chamberRuntime.ContainerResultStatusUnknown:
	default:
		return fmt.Errorf("%w: unknown supervisor result status %q", chamberErrors.ErrInvalidRequest, result.Status)
	}
	if result.ExitedAt.IsZero() {
		return fmt.Errorf("%w: supervisor result exit time is required", chamberErrors.ErrInvalidRequest)
	}
	if result.Status == chamberRuntime.ContainerResultStatusExited && result.ExitCode == nil {
		return fmt.Errorf("%w: exited supervisor result requires an exit code", chamberErrors.ErrInvalidRequest)
	}
	return nil
}

func writeSupervisorJSON(path string, value any) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: supervisor file path is required", chamberErrors.ErrInvalidRequest)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: create supervisor file directory: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("%w: create temporary supervisor file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: encode supervisor file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: sync supervisor file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: close supervisor file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("%w: set supervisor file mode: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("%w: commit supervisor file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	committed = true
	return nil
}

func readSupervisorJSON[T any](path string) (T, bool, error) {
	var zero T
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, fmt.Errorf("%w: read supervisor file %q: %w", chamberErrors.ErrFilesystemFailed, path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var value T
	if err := decoder.Decode(&value); err != nil {
		return zero, true, fmt.Errorf("%w: decode supervisor file %q: %w", chamberErrors.ErrMetadataFailed, path, err)
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return zero, true, fmt.Errorf("%w: supervisor file %q must contain one JSON object", chamberErrors.ErrMetadataFailed, path)
	} else if !errors.Is(err, io.EOF) {
		return zero, true, fmt.Errorf("%w: decode supervisor file %q: %w", chamberErrors.ErrMetadataFailed, path, err)
	}
	return value, true, nil
}
