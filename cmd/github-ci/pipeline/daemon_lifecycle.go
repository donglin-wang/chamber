package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	"github.com/donglin-wang/chamber/pkg/shared/hostprobe"
)

const (
	daemonLifecycleStateRunning = "running"
	daemonLifecycleStateExited  = "exited"
	daemonLifecycleStateFailed  = "failed"

	daemonLifecycleErrorCanceled          = "canceled"
	daemonLifecycleErrorContainerNonZero  = "container_exit_nonzero"
	daemonLifecycleErrorRuntimeWaitFailed = "runtime_wait_failed"
)

type daemonLifecycleCase struct {
	Name              string            `json:"name"`
	OperationID       string            `json:"operation_id,omitempty"`
	ContainerID       string            `json:"container_id,omitempty"`
	FinalState        string            `json:"final_state,omitempty"`
	ExitCode          *int              `json:"exit_code,omitempty"`
	ErrorCode         string            `json:"error_code,omitempty"`
	SupervisorPID     int               `json:"supervisor_pid,omitempty"`
	SupervisorCommand string            `json:"supervisor_command,omitempty"`
	StdoutPath        string            `json:"stdout_path,omitempty"`
	StderrPath        string            `json:"stderr_path,omitempty"`
	Artifacts         map[string]string `json:"artifacts,omitempty"`
	Cleanup           map[string]bool   `json:"cleanup,omitempty"`
	Polls             []containerRecord `json:"polls,omitempty"`
}

type daemonLifecycleRun struct {
	image       string
	repo        string
	evidenceDir string
	root        string
	binaryPath  string
	configPath  string
	daemonURL   string
	roots       map[string]string
	probe       map[string]any
	startedAt   time.Time
	passed      bool
	failure     string
	cases       map[string]*daemonLifecycleCase
	events      []map[string]any
	cleanup     map[string]bool
}

func RunDaemonLifecycle(ctx context.Context, cfg Config) (exitCode int, runErr error) {
	ctx, cancel, cfg, err := prepareRun(ctx, cfg)
	if err != nil {
		return 1, err
	}
	defer cancel()

	if runtime.GOOS != "linux" {
		return 1, fmt.Errorf("%s pipeline requires Linux", DaemonLifecycle)
	}

	workspace, err := workspacePath(cfg)
	if err != nil {
		return 1, err
	}
	evidenceDir, cleanup, err := prepareEvidenceDir(ctx, cfg)
	if err != nil {
		return 1, err
	}
	defer cleanup()
	chamberRoot, err := daemonLifecycleCreateRoot(cfg.Root)
	if err != nil {
		return 1, err
	}

	run := newDaemonLifecycleRun(cfg.Image, workspace, evidenceDir, chamberRoot)
	defer func() {
		if runErr != nil {
			run.recordFailure(runErr)
		}
		if err := run.writeProof(); err != nil && runErr == nil {
			runErr = err
			exitCode = 1
		}
	}()

	if err := run.recordDaemonProbe(ctx); err != nil {
		return 1, err
	}

	if err := run.buildDaemon(ctx); err != nil {
		return 1, err
	}

	httpAddr, err := daemonLifecycleFreeHTTPAddr()
	if err != nil {
		return 1, err
	}
	if err := run.writeDaemonConfig(httpAddr); err != nil {
		return 1, err
	}

	stopDaemon, err := run.startDaemon(ctx, "initial")
	if err != nil {
		return 1, err
	}
	defer func() {
		if err := stopDaemon(); err != nil && runErr == nil {
			runErr = err
			exitCode = 1
		}
	}()

	if err := run.pullImage(ctx); err != nil {
		return 1, err
	}

	success, err := run.runCase(ctx, "short-success", []string{"/bin/sh", "-c", "echo ok; echo err-ok >&2"})
	if err != nil {
		return 1, err
	}
	if err := run.waitTerminal(ctx, success); err != nil {
		return 1, err
	}
	if err := run.readLogs(ctx, success); err != nil {
		return 1, err
	}
	if err := daemonLifecycleRequireExit(success, daemonLifecycleStateExited, 0, ""); err != nil {
		return 1, err
	}

	failing, err := run.runCase(ctx, "nonzero-failure", []string{"/bin/sh", "-c", "echo fail-stderr >&2; exit 7"})
	if err != nil {
		return 1, err
	}
	if err := run.waitTerminal(ctx, failing); err != nil {
		return 1, err
	}
	if err := run.readLogs(ctx, failing); err != nil {
		return 1, err
	}
	if err := daemonLifecycleRequireExit(failing, daemonLifecycleStateExited, 7, daemonLifecycleErrorContainerNonZero); err != nil {
		return 1, err
	}

	restart, err := run.runCase(ctx, "daemon-restart-running-supervisor", []string{"/bin/sh", "-c", "echo restart-started; sleep 6; echo restart-done"})
	if err != nil {
		return 1, err
	}
	if err := run.waitState(ctx, restart, daemonLifecycleStateRunning); err != nil {
		return 1, err
	}
	if err := run.recordSupervisorProcess(ctx, restart); err != nil {
		return 1, err
	}
	if err := stopDaemon(); err != nil {
		return 1, err
	}
	stopDaemon, err = run.startDaemon(ctx, "after-restart")
	if err != nil {
		return 1, err
	}
	if err := run.waitTerminal(ctx, restart); err != nil {
		return 1, err
	}
	if err := run.readLogs(ctx, restart); err != nil {
		return 1, err
	}
	if err := daemonLifecycleRequireExit(restart, daemonLifecycleStateExited, 0, ""); err != nil {
		return 1, err
	}
	if err := daemonLifecycleRequireLogContains(restart.StdoutPath, "restart-done"); err != nil {
		return 1, err
	}

	killed, err := run.runCase(ctx, "killed-supervisor-recovery", []string{"/bin/sh", "-c", "echo supervisor-started; sleep 30"})
	if err != nil {
		return 1, err
	}
	if err := run.waitState(ctx, killed, daemonLifecycleStateRunning); err != nil {
		return 1, err
	}
	if err := run.recordSupervisorProcess(ctx, killed); err != nil {
		return 1, err
	}
	if killed.SupervisorPID == 0 {
		return 1, fmt.Errorf("supervisor PID not found for %s", killed.Name)
	}
	if err := syscall.Kill(killed.SupervisorPID, syscall.SIGKILL); err != nil {
		return 1, fmt.Errorf("kill supervisor: %w", err)
	}
	run.recordEvent("killed supervisor", killed.SupervisorPID)
	if err := run.waitTerminal(ctx, killed); err != nil {
		return 1, err
	}
	if err := run.readLogs(ctx, killed); err != nil {
		return 1, err
	}
	if killed.FinalState != daemonLifecycleStateFailed || killed.ErrorCode != daemonLifecycleErrorRuntimeWaitFailed {
		return 1, fmt.Errorf("%s final state=%s error=%s, want failed/%s", killed.Name, killed.FinalState, killed.ErrorCode, daemonLifecycleErrorRuntimeWaitFailed)
	}

	canceled, err := run.runCase(ctx, "cancel-forced-cleanup", []string{"/bin/sh", "-c", "echo cancel-started; sleep 30"})
	if err != nil {
		return 1, err
	}
	if err := run.waitState(ctx, canceled, daemonLifecycleStateRunning); err != nil {
		return 1, err
	}
	if err := run.cancelContainer(ctx, canceled); err != nil {
		return 1, err
	}
	if err := run.waitTerminal(ctx, canceled); err != nil {
		return 1, err
	}
	if canceled.FinalState != daemonLifecycleStateFailed || canceled.ErrorCode != daemonLifecycleErrorCanceled {
		return 1, fmt.Errorf("%s final state=%s error=%s, want failed/%s", canceled.Name, canceled.FinalState, canceled.ErrorCode, daemonLifecycleErrorCanceled)
	}
	if err := daemonLifecycleVerifyArtifactsRemoved(canceled); err != nil {
		return 1, err
	}

	for _, c := range []*daemonLifecycleCase{success, failing, restart, killed, canceled} {
		if err := run.removeContainer(ctx, c); err != nil {
			return 1, err
		}
		if err := daemonLifecycleVerifyArtifactsRemoved(c); err != nil {
			return 1, err
		}
	}

	if err := stopDaemon(); err != nil {
		return 1, err
	}
	if cfg.Keep {
		run.cleanup["chamber_root_removed"] = false
	} else if err := daemonLifecycleRemoveRoot(run.root); err != nil {
		run.cleanup["chamber_root_removed"] = false
		return 1, fmt.Errorf("remove chamber root: %w", err)
	} else {
		run.cleanup["chamber_root_removed"] = true
	}
	run.passed = true
	return 0, nil
}

func daemonLifecycleRoots(root string) map[string]string {
	return map[string]string{
		"root":        root,
		"tmp":         filepath.Join(root, "tmp"),
		"images":      filepath.Join(root, "images"),
		"bundles":     filepath.Join(root, "bundles"),
		"runtime":     filepath.Join(root, "runtime"),
		"runtime_bin": filepath.Join(root, "bin"),
		"metadata":    filepath.Join(root, "metadata"),
		"supervisors": filepath.Join(root, "supervisors"),
	}
}

func daemonLifecycleCreateRoot(root string) (string, error) {
	rootParent, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve CI root: %w", err)
	}
	parent := filepath.Join(rootParent, "dl")
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", fmt.Errorf("create daemon lifecycle root parent: %w", err)
	}
	if err := os.Chmod(parent, 0700); err != nil {
		return "", fmt.Errorf("make daemon lifecycle root parent private: %w", err)
	}
	chamberRoot, err := os.MkdirTemp(parent, "r-*")
	if err != nil {
		return "", fmt.Errorf("create daemon lifecycle root: %w", err)
	}
	return chamberRoot, nil
}

func newDaemonLifecycleRun(image string, repo string, evidenceDir string, root string) *daemonLifecycleRun {
	return &daemonLifecycleRun{
		image:       image,
		repo:        repo,
		evidenceDir: evidenceDir,
		root:        root,
		roots:       daemonLifecycleRoots(root),
		startedAt:   time.Now().UTC(),
		cases:       make(map[string]*daemonLifecycleCase),
		cleanup:     make(map[string]bool),
	}
}

func (r *daemonLifecycleRun) writeProof() error {
	proof := map[string]any{
		"passed":       r.passed,
		"started_at":   r.startedAt,
		"finished_at":  time.Now().UTC(),
		"binary_path":  r.binaryPath,
		"config_path":  r.configPath,
		"daemon_url":   r.daemonURL,
		"image":        r.image,
		"repo":         r.repo,
		"evidence_dir": r.evidenceDir,
		"git":          gitEvidence(r.repo),
		"host":         hostEvidence(),
		"roots":        r.roots,
		"daemon_probe": r.probe,
		"cases":        r.cases,
		"events":       r.events,
		"cleanup":      r.cleanup,
	}
	if r.failure != "" {
		proof["failure"] = r.failure
	}
	return daemonLifecycleWriteJSON(filepath.Join(r.evidenceDir, "proof.json"), proof)
}

func (r *daemonLifecycleRun) recordFailure(err error) {
	if err == nil {
		return
	}
	r.passed = false
	if r.failure == "" {
		r.failure = err.Error()
	}
}

func (r *daemonLifecycleRun) recordDaemonProbe(ctx context.Context) error {
	probe, passed := daemonLifecycleRunDaemonProbe(ctx)
	r.probe = probe
	if err := daemonLifecycleWriteJSON(filepath.Join(r.evidenceDir, "daemon-probe.json"), probe); err != nil {
		return err
	}
	if !passed {
		return fmt.Errorf("%s daemon host probe failed", DaemonLifecycle)
	}
	return nil
}

func (r *daemonLifecycleRun) buildDaemon(ctx context.Context) error {
	r.binaryPath = filepath.Join(r.evidenceDir, "chamberd")
	return daemonLifecycleBuildDaemon(ctx, r.repo, r.binaryPath, filepath.Join(r.evidenceDir, "build.log"))
}

func (r *daemonLifecycleRun) writeDaemonConfig(httpAddr string) error {
	r.daemonURL = "http://" + httpAddr
	r.configPath = filepath.Join(r.evidenceDir, "daemon-config.json")
	return daemonLifecycleWriteDaemonConfig(r.configPath, httpAddr, r.roots)
}

func (r *daemonLifecycleRun) startDaemon(ctx context.Context, label string) (func() error, error) {
	stdoutPath := filepath.Join(r.evidenceDir, "daemon-"+label+".stdout.log")
	stderrPath := filepath.Join(r.evidenceDir, "daemon-"+label+".stderr.log")
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		return nil, fmt.Errorf("create daemon stdout: %w", err)
	}
	stderr, err := os.Create(stderrPath)
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("create daemon stderr: %w", err)
	}
	command := exec.CommandContext(ctx, r.binaryPath, "serve", "-config", r.configPath)
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	pid := command.Process.Pid
	stopped := false
	stopDaemon := func() error {
		if stopped {
			return nil
		}
		stopped = true

		var err error
		if command.ProcessState == nil || !command.ProcessState.Exited() {
			_ = command.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case waitErr := <-done:
				var exitErr *exec.ExitError
				if waitErr != nil && !errors.As(waitErr, &exitErr) {
					err = fmt.Errorf("wait daemon %s: %w", label, waitErr)
				}
			case <-time.After(10 * time.Second):
				_ = command.Process.Kill()
				waitErr := <-done
				var exitErr *exec.ExitError
				if waitErr != nil && !errors.As(waitErr, &exitErr) {
					err = fmt.Errorf("wait killed daemon %s: %w", label, waitErr)
				}
			}
		}
		if closeErr := stdout.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close daemon stdout: %w", closeErr)
		}
		if closeErr := stderr.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close daemon stderr: %w", closeErr)
		}
		r.recordEvent("stopped daemon "+label, pid)
		return err
	}
	r.recordEvent("started daemon "+label, command.Process.Pid)
	if err := daemonLifecycleWaitHealth(ctx, r.daemonURL); err != nil {
		stopErr := stopDaemon()
		return nil, errors.Join(daemonLifecycleStartFailure(err, stdoutPath, stderrPath), stopErr)
	}
	return stopDaemon, nil
}

func (r *daemonLifecycleRun) pullImage(ctx context.Context) error {
	_, err := daemonLifecyclePostJSON(ctx, r.daemonURL+"/v1/images/pull", map[string]string{"reference": r.image}, filepath.Join(r.evidenceDir, "pull.json"))
	return err
}

func (r *daemonLifecycleRun) runCase(ctx context.Context, name string, command []string) (*daemonLifecycleCase, error) {
	caseDir := filepath.Join(r.evidenceDir, name)
	if err := os.MkdirAll(caseDir, 0700); err != nil {
		return nil, fmt.Errorf("create case dir: %w", err)
	}
	record, err := daemonLifecyclePostJSON(ctx, r.daemonURL+"/v1/containers/run", map[string]any{
		"image":   r.image,
		"command": command,
	}, filepath.Join(caseDir, "run.json"))
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", name, err)
	}
	var response runResponse
	if err := json.Unmarshal(record, &response); err != nil {
		return nil, fmt.Errorf("decode run response: %w", err)
	}
	c := &daemonLifecycleCase{
		Name:        name,
		OperationID: response.OperationID,
		ContainerID: response.ID,
		Artifacts: map[string]string{
			"bundle":        filepath.Join(r.roots["bundles"], response.ID),
			"runtime":       filepath.Join(r.roots["runtime"], response.ID),
			"runtimeStdout": filepath.Join(r.roots["runtime"], "logs", response.ID, string(chamberRuntime.StdoutLogStream)+".log"),
			"runtimeStderr": filepath.Join(r.roots["runtime"], "logs", response.ID, string(chamberRuntime.StderrLogStream)+".log"),
			"supervisor":    filepath.Join(r.roots["supervisors"], response.ID),
		},
	}
	r.cases[name] = c
	return c, nil
}

func (r *daemonLifecycleRun) waitState(ctx context.Context, c *daemonLifecycleCase, state string) error {
	return r.waitForContainer(ctx, c, func(container containerRecord) bool {
		return container.State == state
	})
}

func (r *daemonLifecycleRun) waitTerminal(ctx context.Context, c *daemonLifecycleCase) error {
	return r.waitForContainer(ctx, c, func(container containerRecord) bool {
		return container.State == daemonLifecycleStateExited || container.State == daemonLifecycleStateFailed
	})
}

func (r *daemonLifecycleRun) waitForContainer(ctx context.Context, c *daemonLifecycleCase, done func(containerRecord) bool) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()

	caseDir := filepath.Join(r.evidenceDir, c.Name)
	for {
		body, err := daemonLifecycleGetBytes(ctx, r.daemonURL+"/v1/containers/"+c.ContainerID, "")
		if err != nil {
			return fmt.Errorf("get container %s: %w", c.ContainerID, err)
		}
		var container containerRecord
		if err := json.Unmarshal(body, &container); err != nil {
			return fmt.Errorf("decode container: %w", err)
		}
		c.Polls = append(c.Polls, container)
		c.FinalState = container.State
		c.ExitCode = container.ExitCode
		c.ErrorCode = container.ErrorCode
		_ = daemonLifecycleWriteJSON(filepath.Join(caseDir, "polls.json"), c.Polls)
		_ = daemonLifecycleWriteJSON(filepath.Join(caseDir, "container.json"), container)
		if done(container) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for container %s: %w", c.ContainerID, ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("wait for container %s timed out at state %s", c.ContainerID, c.FinalState)
		case <-ticker.C:
		}
	}
}

func (r *daemonLifecycleRun) readLogs(ctx context.Context, c *daemonLifecycleCase) error {
	caseDir := filepath.Join(r.evidenceDir, c.Name)
	stdoutPath := filepath.Join(caseDir, "stdout.log")
	stderrPath := filepath.Join(caseDir, "stderr.log")
	if _, err := daemonLifecycleGetBytes(ctx, r.daemonURL+"/v1/containers/"+c.ContainerID+"/logs?stream=stdout", stdoutPath); err != nil {
		return fmt.Errorf("read stdout for %s: %w", c.Name, err)
	}
	if _, err := daemonLifecycleGetBytes(ctx, r.daemonURL+"/v1/containers/"+c.ContainerID+"/logs?stream=stderr", stderrPath); err != nil {
		return fmt.Errorf("read stderr for %s: %w", c.Name, err)
	}
	c.StdoutPath = stdoutPath
	c.StderrPath = stderrPath
	return nil
}

func (r *daemonLifecycleRun) recordSupervisorProcess(ctx context.Context, c *daemonLifecycleCase) error {
	supervisorPath := filepath.Join(r.roots["supervisors"], c.ContainerID, "supervisor.json")
	pid, command, err := daemonLifecycleFindSupervisorProcess(ctx, supervisorPath)
	if err != nil {
		return fmt.Errorf("find supervisor process: %w", err)
	}
	c.SupervisorPID = pid
	c.SupervisorCommand = command
	if !strings.Contains(command, "runtime-supervisor") || !strings.Contains(command, supervisorPath) {
		return fmt.Errorf("supervisor command %q does not prove runtime-supervisor mode for %s", command, supervisorPath)
	}
	return nil
}

func (r *daemonLifecycleRun) cancelContainer(ctx context.Context, c *daemonLifecycleCase) error {
	_, err := daemonLifecyclePostJSON(ctx, r.daemonURL+"/v1/containers/"+c.ContainerID+"/cancel", map[string]string{}, filepath.Join(r.evidenceDir, c.Name, "cancel.json"))
	return err
}

func (r *daemonLifecycleRun) removeContainer(ctx context.Context, c *daemonLifecycleCase) error {
	if _, err := daemonLifecycleDoJSON(ctx, http.MethodDelete, r.daemonURL+"/v1/containers/"+c.ContainerID, nil, filepath.Join(r.evidenceDir, c.Name, "delete.json")); err != nil {
		return fmt.Errorf("delete container %s: %w", c.ContainerID, err)
	}
	if _, err := daemonLifecycleGetBytes(ctx, r.daemonURL+"/v1/containers/"+c.ContainerID, ""); err == nil {
		return fmt.Errorf("container %s still exists after delete", c.ContainerID)
	}
	return nil
}

func (r *daemonLifecycleRun) recordEvent(name string, pid int) {
	r.events = append(r.events, daemonLifecycleEvent(name, pid))
}

func daemonLifecycleRunDaemonProbe(ctx context.Context) (map[string]any, bool) {
	rules := []hostprobe.Rule{
		hostprobe.RequireLinux,
		hostprobe.RequireRootlessUser,
		hostprobe.RequireUserNamespacesEnabled,
		hostprobe.RequireAppArmorAllowsUserNamespaces,
		hostprobe.ProbeUserNamespace,
	}
	passed := true
	results := make([]map[string]any, 0, len(rules))
	for _, rule := range rules {
		findings := rule.Check(ctx)
		if len(findings) > 0 {
			passed = false
		}
		result := map[string]any{"name": rule.Name()}
		if len(findings) > 0 {
			result["findings"] = findings
		}
		results = append(results, result)
	}
	return map[string]any{
		"passed": passed,
		"rules":  results,
	}, passed
}

func daemonLifecycleBuildDaemon(ctx context.Context, repoRoot string, binaryPath string, logPath string) error {
	command := exec.CommandContext(ctx, "go", "build", "-o", binaryPath, "./daemon")
	command.Dir = repoRoot
	output, err := command.CombinedOutput()
	if writeErr := os.WriteFile(logPath, output, 0600); writeErr != nil && err == nil {
		err = writeErr
	}
	if err != nil {
		return fmt.Errorf("go build ./daemon: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func daemonLifecycleWriteDaemonConfig(path string, httpAddr string, roots map[string]string) error {
	config := map[string]any{
		"http_addr": httpAddr,
		"tmp_root":  roots["tmp"],
		"image": map[string]string{
			"root": roots["images"],
		},
		"bundle": map[string]string{
			"root": roots["bundles"],
		},
		"runtime": map[string]string{
			"runtime_root":    roots["runtime"],
			"runtime_bin_dir": roots["runtime_bin"],
		},
		"metadata": map[string]string{
			"root": roots["metadata"],
		},
		"logging": map[string]string{
			"level":  "error",
			"format": "text",
		},
	}
	return daemonLifecycleWriteJSON(path, config)
}

func daemonLifecycleEvent(name string, pid int) map[string]any {
	event := map[string]any{
		"name": name,
		"at":   time.Now().UTC(),
	}
	if pid > 0 {
		event["pid"] = pid
	}
	return event
}

func daemonLifecycleRequireExit(c *daemonLifecycleCase, state string, exitCode int, errorCode string) error {
	if c.FinalState != state {
		return fmt.Errorf("%s final state = %s, want %s", c.Name, c.FinalState, state)
	}
	if c.ExitCode == nil || *c.ExitCode != exitCode {
		return fmt.Errorf("%s exit code = %v, want %d", c.Name, c.ExitCode, exitCode)
	}
	if c.ErrorCode != errorCode {
		return fmt.Errorf("%s error code = %q, want %q", c.Name, c.ErrorCode, errorCode)
	}
	return nil
}

func daemonLifecycleRequireLogContains(path string, pattern string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read log %s: %w", path, err)
	}
	if !bytes.Contains(content, []byte(pattern)) {
		return fmt.Errorf("log %s does not contain %q: %s", path, pattern, string(content))
	}
	return nil
}

func daemonLifecycleFindSupervisorProcess(ctx context.Context, supervisorPath string) (int, string, error) {
	output, err := exec.CommandContext(ctx, "ps", "-eo", "pid=,args=").CombinedOutput()
	if err != nil {
		return 0, "", fmt.Errorf("ps: %w: %s", err, strings.TrimSpace(string(output)))
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "runtime-supervisor") || !strings.Contains(line, supervisorPath) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		return pid, strings.TrimSpace(strings.TrimPrefix(line, fields[0])), nil
	}
	return 0, "", fmt.Errorf("supervisor process for %s not found", supervisorPath)
}

func daemonLifecycleVerifyArtifactsRemoved(c *daemonLifecycleCase) error {
	c.Cleanup = make(map[string]bool)
	for name, path := range c.Artifacts {
		_, err := os.Stat(path)
		removed := os.IsNotExist(err)
		c.Cleanup[name] = removed
		if !removed {
			return fmt.Errorf("%s artifact %s remains at %s (stat error: %v)", c.Name, name, path, err)
		}
	}
	return nil
}

func daemonLifecycleWaitHealth(ctx context.Context, daemonURL string) error {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := daemonLifecycleGetBytes(ctx, daemonURL+"/healthz", ""); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait health: %w", ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("daemon health did not become ready")
		case <-ticker.C:
		}
	}
}

func daemonLifecycleStartFailure(err error, stdoutPath string, stderrPath string) error {
	details := []string{err.Error()}
	if stderr := daemonLifecycleLogSnippet(stderrPath); stderr != "" {
		details = append(details, "stderr: "+stderr)
	}
	if stdout := daemonLifecycleLogSnippet(stdoutPath); stdout != "" {
		details = append(details, "stdout: "+stdout)
	}
	details = append(details, "stderr_log: "+stderrPath)
	return errors.New(strings.Join(details, "; "))
}

func daemonLifecycleLogSnippet(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(data))
	if len(text) > 1000 {
		return text[:1000] + "...(truncated)"
	}
	return text
}

func daemonLifecyclePostJSON(ctx context.Context, url string, body any, evidencePath string) ([]byte, error) {
	return daemonLifecycleDoJSON(ctx, http.MethodPost, url, body, evidencePath)
}

func daemonLifecycleDoJSON(ctx context.Context, method string, url string, body any, evidencePath string) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return daemonLifecycleDo(request, evidencePath)
}

func daemonLifecycleGetBytes(ctx context.Context, url string, evidencePath string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return daemonLifecycleDo(request, evidencePath)
}

func daemonLifecycleDo(request *http.Request, evidencePath string) ([]byte, error) {
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if evidencePath != "" {
		if err := os.MkdirAll(filepath.Dir(evidencePath), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(evidencePath, body, 0600); err != nil {
			return nil, err
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return body, fmt.Errorf("%s %s returned HTTP %d: %s", request.Method, request.URL.String(), response.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func daemonLifecycleFreeHTTPAddr() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("find free TCP port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().String(), nil
}

func daemonLifecycleWriteJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return os.WriteFile(path, payload, 0600)
}

func daemonLifecycleRemoveRoot(root string) error {
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("root still exists")
}
