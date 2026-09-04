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
	"strconv"
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
	supervisorLaunchTimeout     = 5 * time.Second

	supervisorPauseAfterRuntimeExitDirEnv = "CHAMBER_TEST_SUPERVISOR_PAUSE_AFTER_RUNTIME_EXIT_DIR"
)

var supervisorProcessMatches = processMatches

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

	RuntimeName         string                          `json:"runtime_name,omitempty"`
	SupervisorPID       int                             `json:"supervisor_pid,omitempty"`
	SupervisorStartTime uint64                          `json:"supervisor_start_time,omitempty"`
	StartedAt           *time.Time                      `json:"started_at,omitempty"`
	Result              *chamberRuntime.ContainerResult `json:"result,omitempty"`

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
	startTime, err := processStartTime(os.Getpid())
	if err != nil {
		return err
	}
	if err := markSupervisorLaunched(path, os.Getpid(), startTime, time.Now().UTC()); err != nil {
		return err
	}
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
	pauseErr := pauseSupervisorAfterRuntimeExit(ctx, state.ContainerID, result)
	return errors.Join(runErr, pauseErr, completeSupervisorFile(path, result))
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
	deadline := time.Now().Add(supervisorLaunchTimeout)
	for {
		state, present, readErr := readSupervisorFile(supervisorPath, "", "")
		if readErr == nil && present && state.SupervisorPID == command.Process.Pid && state.SupervisorStartTime != 0 {
			return command.Process.Pid, command.Wait, nil
		}
		alive, aliveErr := processAlive(command.Process.Pid)
		if aliveErr != nil {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			_ = command.Wait()
			return 0, nil, aliveErr
		}
		if !alive {
			waitErr := command.Wait()
			return 0, nil, fmt.Errorf("%w: supervisor exited before recording process identity: %v", chamberErrors.ErrRuntimeStartFailed, waitErr)
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			_ = command.Wait()
			return 0, nil, fmt.Errorf("%w: supervisor did not record process identity", chamberErrors.ErrRuntimeStartFailed)
		}
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			_ = command.Wait()
			return 0, nil, fmt.Errorf("%w: wait for supervisor process identity: %w", chamberErrors.ErrCanceled, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
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
			var err error
			daemonOperationLocks.with("container:"+containerID, func() {
				err = reconcileSupervisorContainerID(ctx, store, containerID, nil, nil, chamberRuntime.Config{}, false)
			})
			if err != nil && !errors.Is(err, metadata.ErrNotFound) {
				fmt.Fprintf(os.Stderr, "record supervisor state: operation=%s container=%s error=%v\n", operationID, containerID, err)
			}
		case waitErr := <-waitDone:
			var err error
			daemonOperationLocks.with("container:"+containerID, func() {
				err = recordSupervisorExit(ctx, store, operationID, containerID, supervisorPath, waitErr)
			})
			if err != nil && !errors.Is(err, metadata.ErrNotFound) {
				fmt.Fprintf(os.Stderr, "record supervisor result: operation=%s container=%s error=%v\n", operationID, containerID, err)
			}
			return
		}
	}
}

func watchSupervisorContainers(
	ctx context.Context,
	store metadata.Store,
	startSupervisor startSupervisorFunc,
	openContainer openContainerFunc,
	runtimeConfig chamberRuntime.Config,
) {
	if err := reconcileSupervisorContainers(ctx, store, startSupervisor, openContainer, runtimeConfig, false); err != nil {
		fmt.Fprintf(os.Stderr, "reconcile supervisor containers: error=%v\n", err)
	}

	ticker := time.NewTicker(supervisorReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := reconcileSupervisorContainers(ctx, store, startSupervisor, openContainer, runtimeConfig, false); err != nil {
				fmt.Fprintf(os.Stderr, "reconcile supervisor containers: error=%v\n", err)
			}
		}
	}
}

func reconcileSupervisorContainers(
	ctx context.Context,
	store metadata.Store,
	startSupervisor startSupervisorFunc,
	openContainer openContainerFunc,
	runtimeConfig chamberRuntime.Config,
	startup bool,
) error {
	containers, err := store.ListContainers(ctx)
	if err != nil {
		return err
	}
	var reconcileErr error
	for _, container := range containers {
		if container.State != metadata.ContainerCreating &&
			container.State != metadata.ContainerCreated &&
			container.State != metadata.ContainerStarting &&
			container.State != metadata.ContainerRunning {
			continue
		}
		var err error
		daemonOperationLocks.with("container:"+container.ID, func() {
			err = reconcileSupervisorContainerID(ctx, store, container.ID, startSupervisor, openContainer, runtimeConfig, startup)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "reconcile supervisor container: operation=%s container=%s error=%v\n", container.OperationID, container.ID, err)
			reconcileErr = errors.Join(reconcileErr, err)
		}
	}
	return reconcileErr
}

func reconcileSupervisorContainerID(
	ctx context.Context,
	store metadata.Store,
	containerID string,
	startSupervisor startSupervisorFunc,
	openContainer openContainerFunc,
	runtimeConfig chamberRuntime.Config,
	startup bool,
) error {
	container, err := store.GetContainer(ctx, containerID)
	if err != nil {
		return err
	}
	return reconcileSupervisorContainer(ctx, store, container, startSupervisor, openContainer, runtimeConfig, startup)
}

func reconcileSupervisorContainer(
	ctx context.Context,
	store metadata.Store,
	container metadata.Container,
	startSupervisor startSupervisorFunc,
	openContainer openContainerFunc,
	runtimeConfig chamberRuntime.Config,
	startup bool,
) error {
	if strings.TrimSpace(container.SupervisorPath) == "" {
		if startup || container.State != metadata.ContainerCreating || time.Since(container.UpdatedAt) > supervisorCreatingTimeout {
			return failContainerFromCurrent(ctx, store, container.OperationID, container.ID, chamberErrors.ErrRuntimeStartFailed, fmt.Errorf("%s container has no supervisor file path", container.State))
		}
		return nil
	}

	state, present, err := readSupervisorFile(container.SupervisorPath, "", container.ID)
	if err != nil {
		return failContainerFromCurrent(ctx, store, container.OperationID, container.ID, chamberErrors.ErrRuntimeWaitFailed, err)
	}
	if !present {
		if startup || container.State != metadata.ContainerCreating || time.Since(container.UpdatedAt) > supervisorCreatingTimeout {
			return failContainerFromCurrent(ctx, store, container.OperationID, container.ID, chamberErrors.ErrRuntimeStartFailed, fmt.Errorf("%s container supervisor file is missing", container.State))
		}
		return nil
	}
	if state.OperationID != container.OperationID {
		alignedState, adopted, err := alignSupervisorStartOperation(ctx, store, container, state)
		if err != nil {
			return failContainerFromCurrent(ctx, store, container.OperationID, container.ID, chamberErrors.ErrRuntimeWaitFailed, err)
		}
		state = alignedState
		container = adopted
	}

	switch state.Phase {
	case supervisorPrepared:
		return recoverPreparedSupervisor(ctx, store, container, state, startSupervisor, startup)
	case supervisorStarted:
		if state.SupervisorPID != 0 && (container.SupervisorPID != state.SupervisorPID || container.SupervisorStartTime != state.SupervisorStartTime) {
			updated, err := store.SetContainerSupervisor(ctx, container.ID, container.State, state.SupervisorPID, state.SupervisorStartTime, time.Now().UTC())
			if err != nil && !errors.Is(err, chamberErrors.ErrStateConflict) {
				return err
			}
			if err == nil {
				container = updated
			}
		}
		if err := recordSupervisorStarted(ctx, store, container.ID); err != nil {
			return err
		}
		return failContainerIfSupervisorProcessExited(ctx, store, container, state.SupervisorPID, openContainer, runtimeConfig)
	case supervisorCompleted:
		return applySupervisorResult(ctx, store, container.OperationID, container.ID, *state.Result)
	default:
		return failContainerFromCurrent(ctx, store, container.OperationID, container.ID, chamberErrors.ErrRuntimeWaitFailed, fmt.Errorf("unknown supervisor phase %q", state.Phase))
	}
}

func alignSupervisorStartOperation(ctx context.Context, store metadata.Store, container metadata.Container, state supervisorFile) (supervisorFile, metadata.Container, error) {
	if container.State == metadata.ContainerStarting && state.Phase == supervisorPrepared {
		operation, err := store.GetOperation(ctx, container.OperationID)
		if err != nil {
			return state, container, fmt.Errorf("read admitted start operation %q: %w", container.OperationID, err)
		}
		if operation.Kind != metadata.StartOperation || operation.State != metadata.OperationRunning || operation.ResourceID != container.ID {
			return state, container, fmt.Errorf("container operation %q is not a running start operation for container %q", operation.ID, container.ID)
		}
		state.OperationID = operation.ID
		state.SupervisorPID = 0
		state.SupervisorStartTime = 0
		state.RuntimeName = ""
		state.StartedAt = nil
		state.Result = nil
		if err := writeSupervisorFile(container.SupervisorPath, state); err != nil {
			return state, container, err
		}
		return state, container, nil
	}

	operation, err := store.GetOperation(ctx, state.OperationID)
	if err != nil {
		return state, container, fmt.Errorf("read supervisor operation %q: %w", state.OperationID, err)
	}
	if operation.Kind != metadata.StartOperation || operation.State != metadata.OperationRunning || operation.ResourceID != container.ID {
		return state, container, fmt.Errorf("supervisor operation %q is not a running start operation for container %q", operation.ID, container.ID)
	}
	if container.State != metadata.ContainerCreated {
		return state, container, fmt.Errorf("cannot adopt start operation %q from container state %q", operation.ID, container.State)
	}
	if state.Phase == supervisorPrepared {
		container.OperationID = operation.ID
		return state, container, nil
	}
	updated, err := store.TransitionContainer(ctx, container.ID, metadata.ContainerCreated, metadata.ContainerUpdate{
		OperationID:         operation.ID,
		State:               metadata.ContainerStarting,
		At:                  time.Now().UTC(),
		SupervisorPID:       state.SupervisorPID,
		SupervisorStartTime: state.SupervisorStartTime,
		SupervisorPath:      container.SupervisorPath,
		RuntimeRoot:         state.Runtime.RuntimeRoot,
		StdoutPath:          state.StdoutPath,
		StderrPath:          state.StderrPath,
		BundlePath:          state.Bundle.BundlePath,
	})
	return state, updated, err
}

func recoverPreparedSupervisor(
	ctx context.Context,
	store metadata.Store,
	container metadata.Container,
	state supervisorFile,
	startSupervisor startSupervisorFunc,
	startup bool,
) error {
	operation, err := store.GetOperation(ctx, state.OperationID)
	if err != nil {
		return err
	}
	if container.State == metadata.ContainerCreating {
		created, err := store.TransitionContainer(ctx, container.ID, metadata.ContainerCreating, metadata.ContainerUpdate{
			State:          metadata.ContainerCreated,
			At:             time.Now().UTC(),
			BundlePath:     state.Bundle.BundlePath,
			SupervisorPath: container.SupervisorPath,
			RuntimeRoot:    state.Runtime.RuntimeRoot,
			StdoutPath:     state.StdoutPath,
			StderrPath:     state.StderrPath,
		})
		if err != nil {
			return err
		}
		container = created
		if operation.Kind == metadata.CreateOperation {
			_, err := store.SucceedOperation(ctx, operation.ID)
			return ignoreTerminalConflict(err)
		}
	}
	if container.State == metadata.ContainerCreated && operation.Kind == metadata.CreateOperation {
		if operation.State == metadata.OperationRunning {
			_, err := store.SucceedOperation(ctx, operation.ID)
			return ignoreTerminalConflict(err)
		}
		return nil
	}
	if operation.Kind != metadata.RunOperation && operation.Kind != metadata.StartOperation {
		return failContainerFromCurrent(ctx, store, operation.ID, container.ID, chamberErrors.ErrRuntimeStartFailed, fmt.Errorf("prepared supervisor has unsupported operation kind %q", operation.Kind))
	}
	if operation.State != metadata.OperationRunning {
		return failContainerFromCurrent(ctx, store, operation.ID, container.ID, chamberErrors.ErrRuntimeStartFailed, fmt.Errorf("prepared supervisor operation %q is %q", operation.ID, operation.State))
	}
	if state.SupervisorPID == 0 && container.SupervisorPID == 0 && time.Since(state.UpdatedAt) <= supervisorLaunchTimeout {
		// The request that wrote this prepared record may still be between
		// spawning the supervisor and its child recording process identity.
		// Both startup and periodic recovery wait for this narrow handshake
		// window so they cannot launch a duplicate supervisor.
		return nil
	}

	pid := state.SupervisorPID
	startTime := state.SupervisorStartTime
	if pid == 0 {
		pid = container.SupervisorPID
		startTime = container.SupervisorStartTime
	}
	if pid > 0 {
		alive, err := supervisorProcessMatches(pid, startTime)
		if err != nil {
			return err
		}
		if alive {
			if container.State == metadata.ContainerCreated {
				_, err = store.TransitionContainer(ctx, container.ID, metadata.ContainerCreated, metadata.ContainerUpdate{
					OperationID:         operation.ID,
					State:               metadata.ContainerStarting,
					At:                  time.Now().UTC(),
					SupervisorPID:       pid,
					SupervisorStartTime: startTime,
				})
			} else if container.SupervisorPID != pid {
				_, err = store.SetContainerSupervisor(ctx, container.ID, container.State, pid, startTime, time.Now().UTC())
			}
			return ignoreTerminalConflict(err)
		}
	}
	if startSupervisor == nil {
		if startup {
			return fmt.Errorf("supervisor starter is required to recover prepared container %q", container.ID)
		}
		return nil
	}
	pid, waitSupervisor, err := startSupervisor(ctx, container.SupervisorPath)
	if err != nil {
		return failContainerFromCurrent(ctx, store, operation.ID, container.ID, chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeStartFailed), err)
	}
	launchedState, present, readErr := readSupervisorFile(container.SupervisorPath, operation.ID, container.ID)
	if readErr != nil {
		return readErr
	}
	if !present {
		return fmt.Errorf("supervisor file disappeared after recovery launch")
	}
	if container.State == metadata.ContainerCreated {
		_, err = store.TransitionContainer(ctx, container.ID, metadata.ContainerCreated, metadata.ContainerUpdate{
			OperationID:         operation.ID,
			State:               metadata.ContainerStarting,
			At:                  time.Now().UTC(),
			SupervisorPID:       pid,
			SupervisorStartTime: launchedState.SupervisorStartTime,
		})
	} else {
		_, err = store.SetContainerSupervisor(ctx, container.ID, container.State, pid, launchedState.SupervisorStartTime, time.Now().UTC())
	}
	if err != nil {
		return err
	}
	go monitorSupervisor(context.Background(), store, operation.ID, container.ID, container.SupervisorPath, waitSupervisor)
	return nil
}

func failContainerIfSupervisorProcessExited(
	ctx context.Context,
	store metadata.Store,
	container metadata.Container,
	supervisorPID int,
	openContainer openContainerFunc,
	runtimeConfig chamberRuntime.Config,
) error {
	pid := supervisorPID
	startTime := container.SupervisorStartTime
	if pid == 0 {
		pid = container.SupervisorPID
	}
	alive, err := supervisorProcessMatches(pid, startTime)
	if err != nil {
		return err
	}
	if alive {
		return nil
	}
	runtimeEvidence := "runtime state unavailable"
	// Runtime state is liveness evidence only. A completed supervisor result is
	// the durable authority for the container outcome, so a missing result must
	// fail recoverably even when runc still reports a process state.
	if openContainer != nil {
		controlConfig := runtimeConfig
		if strings.TrimSpace(container.RuntimeRoot) != "" {
			controlConfig.RuntimeRoot = container.RuntimeRoot
		}
		handle, openErr := openContainer(ctx, controlConfig, container.ID)
		if openErr != nil {
			runtimeEvidence = fmt.Sprintf("runtime open failed: %v", openErr)
		} else if runtimeState, stateErr := handle.State(ctx); stateErr != nil {
			runtimeEvidence = fmt.Sprintf("runtime state failed: %v", stateErr)
		} else {
			runtimeEvidence = fmt.Sprintf("runtime state is %q", runtimeState.Status)
		}
	}
	return failContainerFromCurrent(
		ctx,
		store,
		container.OperationID,
		container.ID,
		chamberErrors.ErrRuntimeWaitFailed,
		fmt.Errorf("supervisor process %d exited without completed result; %s", pid, runtimeEvidence),
	)
}

func processAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
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

func processMatches(pid int, expectedStartTime uint64) (bool, error) {
	if expectedStartTime == 0 {
		return false, nil
	}
	alive, err := processAlive(pid)
	if err != nil || !alive {
		return alive, err
	}
	observedStartTime, err := processStartTime(pid)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return observedStartTime == expectedStartTime, nil
}

func processStartTime(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("%w: invalid supervisor pid %d", chamberErrors.ErrRuntimeControlFailed, pid)
	}
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("%w: read supervisor process start time: %w", chamberErrors.ErrRuntimeControlFailed, err)
	}
	closingParen := bytes.LastIndexByte(content, ')')
	if closingParen < 0 {
		return 0, fmt.Errorf("%w: malformed supervisor process stat", chamberErrors.ErrRuntimeControlFailed)
	}
	fields := strings.Fields(string(content[closingParen+1:]))
	if len(fields) <= 19 {
		return 0, fmt.Errorf("%w: supervisor process stat has %d fields", chamberErrors.ErrRuntimeControlFailed, len(fields))
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: parse supervisor process start time: %w", chamberErrors.ErrRuntimeControlFailed, err)
	}
	return startTime, nil
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
	if container.State == metadata.ContainerCreating || container.State == metadata.ContainerCreated {
		_, err = store.TransitionContainer(ctx, containerID, container.State, metadata.ContainerUpdate{
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
	container, err := store.GetContainer(ctx, containerID)
	if err != nil {
		return err
	}
	if container.State == metadata.ContainerExited || container.State == metadata.ContainerFailed {
		return nil
	}
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
		operationState := metadata.OperationSucceeded
		if exitCode == 0 {
			return exitContainerFromCurrent(ctx, store, operationID, containerID, exitCode, code, operationState, result.ExitedAt)
		}
		operationState = metadata.OperationFailed
		return exitContainerFromCurrent(ctx, store, operationID, containerID, exitCode, code, operationState, result.ExitedAt)
	case chamberRuntime.ContainerResultStatusStartFailed:
		code := supervisorResultCode(result, chamberErrors.ErrRuntimeStartFailed)
		return failContainerFromCurrent(ctx, store, operationID, containerID, code, errors.New(result.Error))
	case chamberRuntime.ContainerResultStatusCanceled:
		code := supervisorResultCode(result, chamberErrors.ErrCanceled)
		return failOrAbortContainerFromCurrent(ctx, store, operationID, containerID, code, metadata.OperationAborted, result.ExitedAt)
	case chamberRuntime.ContainerResultStatusUnknown:
		code := supervisorResultCode(result, chamberErrors.ErrRuntimeWaitFailed)
		return failContainerFromCurrent(ctx, store, operationID, containerID, code, errors.New(result.Error))
	default:
		return failContainerFromCurrent(ctx, store, operationID, containerID, chamberErrors.ErrRuntimeWaitFailed, fmt.Errorf("unknown supervisor result status %q", result.Status))
	}
}

func exitContainerFromCurrent(
	ctx context.Context,
	store metadata.Store,
	operationID string,
	containerID string,
	exitCode int,
	code chamberErrors.Code,
	operationState metadata.OperationState,
	at time.Time,
) error {
	container, err := store.GetContainer(ctx, containerID)
	if err != nil {
		return err
	}
	switch container.State {
	case metadata.ContainerExited, metadata.ContainerFailed:
		_, err := store.TransitionOperation(ctx, operationID, metadata.OperationRunning, metadata.OperationUpdate{
			State: operationState, At: at, ErrorCode: code,
		})
		return ignoreTerminalConflict(err)
	case metadata.ContainerCreating, metadata.ContainerStarting, metadata.ContainerRunning:
		_, _, err := store.TransitionContainerAndOperation(
			ctx,
			containerID,
			container.State,
			metadata.ContainerUpdate{
				State:           metadata.ContainerExited,
				At:              at,
				ExitCode:        &exitCode,
				ErrorCode:       code,
				ClearSupervisor: true,
			},
			operationID,
			metadata.OperationRunning,
			metadata.OperationUpdate{State: operationState, At: at, ErrorCode: code},
		)
		return err
	default:
		return failContainerFromCurrent(ctx, store, operationID, containerID, chamberErrors.ErrRuntimeWaitFailed, fmt.Errorf("cannot mark container exited from state %q", container.State))
	}
}

func failContainerFromCurrent(ctx context.Context, store metadata.Store, operationID string, containerID string, code chamberErrors.Code, err error) error {
	container, getErr := store.GetContainer(ctx, containerID)
	if getErr != nil {
		return errors.Join(err, getErr)
	}
	if container.State == metadata.ContainerExited || container.State == metadata.ContainerFailed {
		_, operationErr := store.FailOperation(ctx, operationID, code)
		return ignoreTerminalConflict(operationErr)
	}
	_, _, cleanupErr := admitContainerCleanup(ctx, store, containerID, metadata.CleanupOperation, false)
	if errors.Is(cleanupErr, metadata.ErrAlreadyExists) || errors.Is(cleanupErr, chamberErrors.ErrStateConflict) {
		cleanupErr = nil
	}
	_, _, transitionErr := store.FailContainerAndOperation(ctx, containerID, container.State, operationID, code)
	if errors.Is(transitionErr, chamberErrors.ErrStateConflict) {
		transitionErr = failOrAbortContainerFromCurrent(ctx, store, operationID, containerID, code, metadata.OperationFailed, time.Now().UTC())
	}
	recordErr := errors.Join(cleanupErr, transitionErr)
	if recordErr != nil {
		return errors.Join(err, recordErr)
	}
	return nil
}

func failOrAbortContainerFromCurrent(
	ctx context.Context,
	store metadata.Store,
	operationID string,
	containerID string,
	code chamberErrors.Code,
	operationState metadata.OperationState,
	at time.Time,
) error {
	container, err := store.GetContainer(ctx, containerID)
	if err != nil {
		return err
	}
	if container.State == metadata.ContainerExited || container.State == metadata.ContainerFailed {
		_, err := store.TransitionOperation(ctx, operationID, metadata.OperationRunning, metadata.OperationUpdate{
			State: operationState, At: at, ErrorCode: code,
		})
		return ignoreTerminalConflict(err)
	}
	_, _, err = store.TransitionContainerAndOperation(
		ctx,
		containerID,
		container.State,
		metadata.ContainerUpdate{
			State: metadata.ContainerFailed, At: at, ErrorCode: code, ClearSupervisor: true,
		},
		operationID,
		metadata.OperationRunning,
		metadata.OperationUpdate{State: operationState, At: at, ErrorCode: code},
	)
	return ignoreTerminalConflict(err)
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

func markSupervisorLaunched(path string, pid int, startTime uint64, at time.Time) error {
	state, err := readRequiredSupervisorFile(path)
	if err != nil {
		return err
	}
	state.SupervisorPID = pid
	state.SupervisorStartTime = startTime
	state.UpdatedAt = at
	return writeSupervisorFile(path, state)
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

func pauseSupervisorAfterRuntimeExit(ctx context.Context, containerID string, result chamberRuntime.ContainerResult) error {
	dir := strings.TrimSpace(os.Getenv(supervisorPauseAfterRuntimeExitDirEnv))
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: create supervisor pause directory: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	signalPath := filepath.Join(dir, containerID+".runtime-exited.json")
	releasePath := filepath.Join(dir, containerID+".release")
	if err := writeSupervisorJSON(signalPath, map[string]any{
		"container_id": containerID,
		"result":       result,
		"at":           time.Now().UTC(),
	}); err != nil {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(releasePath); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("%w: inspect supervisor pause release file: %w", chamberErrors.ErrFilesystemFailed, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: supervisor pause canceled: %w", chamberErrors.ErrCanceled, ctx.Err())
		case <-ticker.C:
		}
	}
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
