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
	"net/url"
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
	daemonLifecycleStateCreated        = "created"
	daemonLifecycleStateStarting       = "starting"
	daemonLifecycleStateRunning        = "running"
	daemonLifecycleStateExited         = "exited"
	daemonLifecycleStateFailed         = "failed"
	daemonLifecycleStateDecommissioned = "decommissioned"

	daemonLifecycleErrorContainerNonZero   = "container_exit_nonzero"
	daemonLifecycleErrorRuntimeStartFailed = "runtime_start_failed"
	daemonLifecycleErrorRuntimeWaitFailed  = "runtime_wait_failed"

	daemonLifecycleSupervisorPauseAfterRuntimeExitDirEnv = "CHAMBER_TEST_SUPERVISOR_PAUSE_AFTER_RUNTIME_EXIT_DIR"
	daemonLifecyclePauseDirEnv                           = "CHAMBER_TEST_DAEMON_LIFECYCLE_PAUSE_DIR"
	daemonLifecyclePausePointEnv                         = "CHAMBER_TEST_DAEMON_LIFECYCLE_PAUSE_POINT"
	daemonLifecycleSupervisorPhaseCompleted              = "completed"
	daemonLifecyclePauseAfterCreateAdmission             = "after-create-admission"
	daemonLifecyclePauseAfterStartAdmission              = "after-start-admission"

	daemonLifecycleContainerTimeout = 5 * time.Minute
)

type daemonLifecycleCase struct {
	Name              string                   `json:"name"`
	OperationID       string                   `json:"operation_id,omitempty"`
	CreateOperationID string                   `json:"create_operation_id,omitempty"`
	StartOperationID  string                   `json:"start_operation_id,omitempty"`
	ContainerID       string                   `json:"container_id,omitempty"`
	FinalState        string                   `json:"final_state,omitempty"`
	ExitCode          *int                     `json:"exit_code,omitempty"`
	ErrorCode         string                   `json:"error_code,omitempty"`
	SupervisorPID     int                      `json:"supervisor_pid,omitempty"`
	SupervisorCommand string                   `json:"supervisor_command,omitempty"`
	StdoutPath        string                   `json:"stdout_path,omitempty"`
	StderrPath        string                   `json:"stderr_path,omitempty"`
	Artifacts         map[string]string        `json:"artifacts,omitempty"`
	Decommission      map[string]bool          `json:"decommission_artifact_removed,omitempty"`
	Cleanup           map[string]bool          `json:"cleanup,omitempty"`
	Polls             []containerRecord        `json:"polls,omitempty"`
	ListPolls         [][]containerRecord      `json:"list_polls,omitempty"`
	LogPolls          []daemonLifecycleLogPoll `json:"log_polls,omitempty"`
}

type daemonLifecycleLogPoll struct {
	At     time.Time `json:"at"`
	Stream string    `json:"stream"`
	Bytes  int       `json:"bytes"`
}

type daemonLifecycleOperation struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	State string `json:"state"`
}

type daemonLifecycleOperationsResponse struct {
	Operations      []daemonLifecycleOperation `json:"operations"`
	PendingCleanups int                        `json:"pending_cleanups"`
}

type daemonLifecycleRun struct {
	image               string
	repo                string
	evidenceDir         string
	root                string
	binaryPath          string
	binarySHA256        string
	sourceSHA256        string
	configPath          string
	daemonURL           string
	pauseDir            string
	admissionPauseDir   string
	admissionPausePoint string
	roots               map[string]string
	probe               map[string]any
	startedAt           time.Time
	passed              bool
	failure             string
	cases               map[string]*daemonLifecycleCase
	events              []map[string]any
	cleanup             map[string]bool
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
	if err := run.recordDaemonStartupProbe(ctx); err != nil {
		return 1, err
	}

	if err := run.pullImage(ctx); err != nil {
		return 1, err
	}

	creatingCrash, err := run.runCreateAdmissionCrashCase(ctx, &stopDaemon)
	if err != nil {
		return 1, err
	}
	earlyStartCrash, err := run.runStartAdmissionCrashCase(ctx, &stopDaemon)
	if err != nil {
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

	separate, err := run.createCase(ctx, "create-start-separate", []string{"/bin/sh", "-c", "echo separate-started; sleep 1; echo separate-done"})
	if err != nil {
		return 1, err
	}
	if err := run.waitState(ctx, separate, daemonLifecycleStateCreated); err != nil {
		return 1, err
	}
	if err := run.startContainer(ctx, separate); err != nil {
		return 1, err
	}
	if err := run.waitTerminal(ctx, separate); err != nil {
		return 1, err
	}
	if err := run.readLogs(ctx, separate); err != nil {
		return 1, err
	}
	if err := daemonLifecycleRequireExit(separate, daemonLifecycleStateExited, 0, ""); err != nil {
		return 1, err
	}
	if err := daemonLifecycleRequireLogContains(separate.StdoutPath, "separate-done"); err != nil {
		return 1, err
	}

	starting, err := run.createCase(ctx, "daemon-restart-starting", []string{"/bin/sh", "-c", "echo starting-restart; sleep 4; echo starting-done"})
	if err != nil {
		return 1, err
	}
	if err := run.startContainer(ctx, starting); err != nil {
		return 1, err
	}
	if starting.FinalState != daemonLifecycleStateStarting {
		return 1, fmt.Errorf("%s state after start = %s, want %s", starting.Name, starting.FinalState, daemonLifecycleStateStarting)
	}
	if err := stopDaemon(); err != nil {
		return 1, err
	}
	stopDaemon, err = run.startDaemon(ctx, "after-starting-restart")
	if err != nil {
		return 1, err
	}
	if err := run.waitTerminal(ctx, starting); err != nil {
		return 1, err
	}
	if err := run.readLogs(ctx, starting); err != nil {
		return 1, err
	}
	if err := daemonLifecycleRequireExit(starting, daemonLifecycleStateExited, 0, ""); err != nil {
		return 1, err
	}

	testSuite, err := run.runCaseWithMounts(ctx, "chamber-test-suite-daemon-restart", []string{"/bin/sh", "-c", "cd /workspace && " + GoTestShellCommand}, []map[string]any{{
		"type":    "bind",
		"source":  workspace,
		"target":  "/workspace",
		"options": []string{"rbind", "ro"},
	}})
	if err != nil {
		return 1, err
	}
	if err := run.waitState(ctx, testSuite, daemonLifecycleStateRunning); err != nil {
		return 1, err
	}
	if err := run.recordSupervisorProcess(ctx, testSuite); err != nil {
		return 1, err
	}
	if err := stopDaemon(); err != nil {
		return 1, err
	}
	stopDaemon, err = run.startDaemon(ctx, "after-test-suite-restart")
	if err != nil {
		return 1, err
	}
	if err := run.waitTerminal(ctx, testSuite); err != nil {
		return 1, err
	}
	if err := run.readLogs(ctx, testSuite); err != nil {
		return 1, err
	}
	if err := daemonLifecycleRequireExit(testSuite, daemonLifecycleStateExited, 0, ""); err != nil {
		return 1, err
	}
	if len(testSuite.LogPolls) < 4 {
		return 1, fmt.Errorf("%s live log polls = %d, want repeated stdout/stderr reads", testSuite.Name, len(testSuite.LogPolls))
	}

	runtimeExited, err := run.runRuntimeExitedBeforeDaemonCompletionCase(ctx, &stopDaemon)
	if err != nil {
		return 1, err
	}

	disconnected, err := run.runCaseWithDisconnectedClient(ctx, "client-disconnect-run", []string{"/bin/sh", "-c", "echo disconnected-started; sleep 1; echo disconnected-done"})
	if err != nil {
		return 1, err
	}
	if err := run.waitTerminal(ctx, disconnected); err != nil {
		return 1, err
	}
	if err := run.readLogs(ctx, disconnected); err != nil {
		return 1, err
	}
	if err := daemonLifecycleRequireExit(disconnected, daemonLifecycleStateExited, 0, ""); err != nil {
		return 1, err
	}
	if err := daemonLifecycleRequireLogContains(disconnected.StdoutPath, "disconnected-done"); err != nil {
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
	if err := stopDaemon(); err != nil {
		return 1, err
	}
	if err := syscall.Kill(killed.SupervisorPID, syscall.SIGKILL); err != nil {
		return 1, fmt.Errorf("kill supervisor: %w", err)
	}
	run.recordEvent("killed supervisor while daemon stopped", killed.SupervisorPID)
	stopDaemon, err = run.startDaemon(ctx, "after-killed-supervisor")
	if err != nil {
		return 1, err
	}
	if err := run.waitTerminal(ctx, killed); err != nil {
		return 1, err
	}
	if err := run.readLogs(ctx, killed); err != nil {
		return 1, err
	}
	if killed.FinalState != daemonLifecycleStateFailed || killed.ErrorCode != daemonLifecycleErrorRuntimeWaitFailed {
		return 1, fmt.Errorf("%s final state=%s error=%s, want failed/%s", killed.Name, killed.FinalState, killed.ErrorCode, daemonLifecycleErrorRuntimeWaitFailed)
	}

	decommissioned, err := run.runCase(ctx, "decommission-retains-evidence", []string{"/bin/sh", "-c", "echo decommissioned"})
	if err != nil {
		return 1, err
	}
	if err := run.waitTerminal(ctx, decommissioned); err != nil {
		return 1, err
	}
	if err := run.readLogs(ctx, decommissioned); err != nil {
		return 1, err
	}
	if err := run.decommissionContainer(ctx, decommissioned); err != nil {
		return 1, err
	}
	if err := run.waitState(ctx, decommissioned, daemonLifecycleStateDecommissioned); err != nil {
		return 1, err
	}
	if err := run.readLogs(ctx, decommissioned); err != nil {
		return 1, err
	}
	if err := daemonLifecycleRequireLogContains(decommissioned.StdoutPath, "decommissioned"); err != nil {
		return 1, err
	}
	if err := daemonLifecycleVerifyDecommissioned(decommissioned); err != nil {
		return 1, err
	}
	if err := run.requireStartRejected(ctx, decommissioned); err != nil {
		return 1, err
	}

	forceDeleted, err := run.runCase(ctx, "force-delete-running-container", []string{"/bin/sh", "-c", "echo force-delete-started; sleep 30"})
	if err != nil {
		return 1, err
	}
	if err := run.waitState(ctx, forceDeleted, daemonLifecycleStateRunning); err != nil {
		return 1, err
	}
	if err := run.deleteContainer(ctx, forceDeleted); err != nil {
		return 1, err
	}
	if err := daemonLifecycleVerifyArtifactsRemoved(forceDeleted); err != nil {
		return 1, err
	}

	stopped, err := run.runCase(ctx, "stop-running-container", []string{"/bin/sh", "-c", "trap 'echo stopped; exit 0' TERM; echo stop-started; while true; do sleep 1; done"})
	if err != nil {
		return 1, err
	}
	if err := run.waitState(ctx, stopped, daemonLifecycleStateRunning); err != nil {
		return 1, err
	}
	if err := run.stopContainer(ctx, stopped); err != nil {
		return 1, err
	}
	if err := run.waitTerminal(ctx, stopped); err != nil {
		return 1, err
	}
	if err := run.readLogs(ctx, stopped); err != nil {
		return 1, err
	}
	if err := daemonLifecycleRequireExit(stopped, daemonLifecycleStateExited, 0, ""); err != nil {
		return 1, err
	}
	if err := daemonLifecycleRequireLogContains(stopped.StdoutPath, "stopped"); err != nil {
		return 1, err
	}

	for _, c := range []*daemonLifecycleCase{creatingCrash, earlyStartCrash, success, failing, separate, starting, testSuite, runtimeExited, disconnected, restart, killed, decommissioned, stopped} {
		if err := run.deleteContainer(ctx, c); err != nil {
			return 1, err
		}
		if err := daemonLifecycleVerifyArtifactsRemoved(c); err != nil {
			return 1, err
		}
	}
	if err := run.auditDurableOperations(ctx); err != nil {
		return 1, err
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
	if err := requireSourceSnapshot(run.repo, run.sourceSHA256); err != nil {
		return 1, err
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
		"passed":                       r.passed,
		"started_at":                   r.startedAt,
		"finished_at":                  time.Now().UTC(),
		"binary_path":                  r.binaryPath,
		"binary_sha256":                r.binarySHA256,
		"build_source_snapshot_sha256": r.sourceSHA256,
		"config_path":                  r.configPath,
		"daemon_url":                   r.daemonURL,
		"image":                        r.image,
		"repo":                         r.repo,
		"evidence_dir":                 r.evidenceDir,
		"git":                          gitEvidence(r.repo),
		"host":                         hostEvidence(),
		"roots":                        r.roots,
		"daemon_probe":                 r.probe,
		"cases":                        r.cases,
		"events":                       r.events,
		"cleanup":                      r.cleanup,
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

func (r *daemonLifecycleRun) recordDaemonStartupProbe(ctx context.Context) error {
	body, err := daemonLifecycleGetBytes(ctx, r.daemonURL+"/v1/system/info", "")
	if err != nil {
		return fmt.Errorf("read daemon startup probe: %w", err)
	}
	var info map[string]any
	if err := json.Unmarshal(body, &info); err != nil {
		return fmt.Errorf("decode daemon startup probe: %w", err)
	}
	startup, ok := info["startup_probe"].(map[string]any)
	if !ok {
		return fmt.Errorf("daemon system info omitted startup_probe")
	}
	passed, _ := startup["passed"].(bool)
	scopes, _ := startup["scopes"].([]any)
	requiredScopes := map[string]bool{
		"image": false, "bundle": false, "runtime": false,
		"metadata": false, "socket": false, "cleanup": false,
	}
	for _, rawScope := range scopes {
		scope, ok := rawScope.(map[string]any)
		if !ok {
			return fmt.Errorf("daemon startup probe contains an invalid scope record")
		}
		name, _ := scope["name"].(string)
		scopePassed, _ := scope["passed"].(bool)
		if _, required := requiredScopes[name]; required && scopePassed {
			requiredScopes[name] = true
		}
	}
	if !passed {
		return fmt.Errorf("daemon startup probe did not pass all selected scopes")
	}
	for name, scopePassed := range requiredScopes {
		if !scopePassed {
			return fmt.Errorf("daemon startup probe omitted passing %s scope", name)
		}
	}
	r.probe["startup"] = startup
	return daemonLifecycleWriteJSON(filepath.Join(r.evidenceDir, "daemon-probe.json"), r.probe)
}

func (r *daemonLifecycleRun) buildDaemon(ctx context.Context) error {
	r.binaryPath = filepath.Join(r.evidenceDir, "chamberd")
	sourceDigest, err := gitSnapshotSHA256(r.repo)
	if err != nil {
		return fmt.Errorf("hash daemon build source: %w", err)
	}
	r.sourceSHA256 = sourceDigest
	if err := daemonLifecycleBuildDaemon(ctx, r.repo, r.binaryPath, filepath.Join(r.evidenceDir, "build.log")); err != nil {
		return err
	}
	if err := requireSourceSnapshot(r.repo, r.sourceSHA256); err != nil {
		return err
	}
	digest, err := fileSHA256(r.binaryPath)
	if err != nil {
		return fmt.Errorf("hash daemon binary: %w", err)
	}
	r.binarySHA256 = digest
	return nil
}

func requireSourceSnapshot(repo string, want string) error {
	if strings.TrimSpace(want) == "" {
		return fmt.Errorf("daemon build source snapshot digest is required")
	}
	got, err := gitSnapshotSHA256(repo)
	if err != nil {
		return fmt.Errorf("rehash daemon build source: %w", err)
	}
	if got != want {
		return fmt.Errorf("daemon build source changed during proof: got sha256:%s, built sha256:%s", got, want)
	}
	return nil
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
	command.Env = os.Environ()
	if r.pauseDir != "" {
		command.Env = append(command.Env, daemonLifecycleSupervisorPauseAfterRuntimeExitDirEnv+"="+r.pauseDir)
	}
	if r.admissionPauseDir != "" && r.admissionPausePoint != "" {
		command.Env = append(command.Env,
			daemonLifecyclePauseDirEnv+"="+r.admissionPauseDir,
			daemonLifecyclePausePointEnv+"="+r.admissionPausePoint,
		)
	}
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
	return r.runCaseWithMounts(ctx, name, command, nil)
}

func (r *daemonLifecycleRun) runCaseWithMounts(ctx context.Context, name string, command []string, mounts []map[string]any) (*daemonLifecycleCase, error) {
	caseDir := filepath.Join(r.evidenceDir, name)
	if err := os.MkdirAll(caseDir, 0700); err != nil {
		return nil, fmt.Errorf("create case dir: %w", err)
	}
	request := map[string]any{
		"image":   r.image,
		"command": command,
	}
	if len(mounts) > 0 {
		request["mounts"] = mounts
	}
	record, err := daemonLifecyclePostJSON(ctx, r.daemonURL+"/v1/containers/run", request, filepath.Join(caseDir, "run.json"))
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", name, err)
	}
	var response runResponse
	if err := json.Unmarshal(record, &response); err != nil {
		return nil, fmt.Errorf("decode run response: %w", err)
	}
	c := r.newCaseFromRunResponse(name, response)
	r.cases[name] = c
	return c, nil
}

func (r *daemonLifecycleRun) createCase(ctx context.Context, name string, command []string) (*daemonLifecycleCase, error) {
	caseDir := filepath.Join(r.evidenceDir, name)
	if err := os.MkdirAll(caseDir, 0700); err != nil {
		return nil, fmt.Errorf("create case dir: %w", err)
	}
	record, err := daemonLifecyclePostJSON(ctx, r.daemonURL+"/v1/containers/create", map[string]any{
		"image":   r.image,
		"command": command,
	}, filepath.Join(caseDir, "create.json"))
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", name, err)
	}
	var response runResponse
	if err := json.Unmarshal(record, &response); err != nil {
		return nil, fmt.Errorf("decode create response: %w", err)
	}
	c := r.newCaseFromRunResponse(name, response)
	c.CreateOperationID = response.OperationID
	r.cases[name] = c
	return c, nil
}

func (r *daemonLifecycleRun) startContainer(ctx context.Context, c *daemonLifecycleCase) error {
	body, err := daemonLifecyclePostJSON(ctx, r.daemonURL+"/v1/containers/"+c.ContainerID+"/start", map[string]string{}, filepath.Join(r.evidenceDir, c.Name, "start.json"))
	if err != nil {
		return err
	}
	var container containerRecord
	if err := json.Unmarshal(body, &container); err != nil {
		return fmt.Errorf("decode start response: %w", err)
	}
	c.OperationID = container.OperationID
	c.StartOperationID = container.OperationID
	c.FinalState = container.State
	c.ExitCode = container.ExitCode
	c.ErrorCode = container.ErrorCode
	return nil
}

func (r *daemonLifecycleRun) runCaseWithDisconnectedClient(ctx context.Context, name string, command []string) (*daemonLifecycleCase, error) {
	caseDir := filepath.Join(r.evidenceDir, name)
	if err := os.MkdirAll(caseDir, 0700); err != nil {
		return nil, fmt.Errorf("create case dir: %w", err)
	}
	before, err := r.listContainerIDs(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.postRunThenDisconnect(ctx, command, filepath.Join(caseDir, "disconnect-request.txt")); err != nil {
		return nil, err
	}
	container, err := r.waitForNewContainer(ctx, before)
	if err != nil {
		return nil, err
	}
	c := r.newCaseFromContainerRecord(name, container)
	r.cases[name] = c
	return c, nil
}

func (r *daemonLifecycleRun) runRuntimeExitedBeforeDaemonCompletionCase(ctx context.Context, stopDaemon *func() error) (*daemonLifecycleCase, error) {
	if stopDaemon == nil || *stopDaemon == nil {
		return nil, fmt.Errorf("runtime-exit restart case requires a running daemon")
	}
	if err := (*stopDaemon)(); err != nil {
		return nil, err
	}

	r.pauseDir = filepath.Join(r.evidenceDir, "supervisor-pause-after-runtime-exit")
	if err := os.MkdirAll(r.pauseDir, 0o700); err != nil {
		return nil, fmt.Errorf("create supervisor pause directory: %w", err)
	}
	nextStopDaemon, err := r.startDaemon(ctx, "with-runtime-exit-pause")
	if err != nil {
		r.pauseDir = ""
		return nil, err
	}
	*stopDaemon = nextStopDaemon

	c, err := r.runCase(ctx, "daemon-restart-after-runtime-exit", []string{"/bin/sh", "-c", "echo runtime-exit-window"})
	if err != nil {
		return nil, err
	}
	signalPath := filepath.Join(r.pauseDir, c.ContainerID+".runtime-exited.json")
	releasePath := filepath.Join(r.pauseDir, c.ContainerID+".release")
	if err := daemonLifecycleWaitFile(ctx, signalPath, 30*time.Second); err != nil {
		return nil, err
	}
	r.recordEvent("runtime exited before daemon completion", 0)
	if err := (*stopDaemon)(); err != nil {
		return nil, err
	}
	if err := os.WriteFile(releasePath, []byte("release\n"), 0o600); err != nil {
		return nil, fmt.Errorf("release paused supervisor: %w", err)
	}
	if err := r.waitSupervisorCompleted(ctx, c, 30*time.Second); err != nil {
		return nil, err
	}

	r.pauseDir = ""
	nextStopDaemon, err = r.startDaemon(ctx, "after-runtime-exit-restart")
	if err != nil {
		return nil, err
	}
	*stopDaemon = nextStopDaemon
	if err := r.waitTerminal(ctx, c); err != nil {
		return nil, err
	}
	if err := r.readLogs(ctx, c); err != nil {
		return nil, err
	}
	if err := daemonLifecycleRequireExit(c, daemonLifecycleStateExited, 0, ""); err != nil {
		return nil, err
	}
	if err := daemonLifecycleRequireLogContains(c.StdoutPath, "runtime-exit-window"); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *daemonLifecycleRun) runCreateAdmissionCrashCase(ctx context.Context, stopDaemon *func() error) (*daemonLifecycleCase, error) {
	if err := (*stopDaemon)(); err != nil {
		return nil, err
	}
	r.admissionPauseDir = filepath.Join(r.evidenceDir, "daemon-admission-pause")
	r.admissionPausePoint = daemonLifecyclePauseAfterCreateAdmission
	if err := os.MkdirAll(r.admissionPauseDir, 0o700); err != nil {
		return nil, fmt.Errorf("create admission pause directory: %w", err)
	}
	nextStopDaemon, err := r.startDaemon(ctx, "pause-after-create-admission")
	if err != nil {
		return nil, err
	}
	*stopDaemon = nextStopDaemon

	name := "daemon-restart-after-create-admission"
	caseDir := filepath.Join(r.evidenceDir, name)
	if err := os.MkdirAll(caseDir, 0o700); err != nil {
		return nil, err
	}
	before, err := r.listContainerIDs(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.postJSONThenDisconnect(ctx, "/v1/containers/run", map[string]any{
		"image": r.image, "command": []string{"/bin/sh", "-c", "echo should-not-start"},
	}, filepath.Join(caseDir, "request.txt")); err != nil {
		return nil, err
	}
	container, err := r.waitForNewContainer(ctx, before)
	if err != nil {
		return nil, err
	}
	c := r.newCaseFromContainerRecord(name, container)
	r.cases[name] = c
	signalPath := filepath.Join(r.admissionPauseDir, c.ContainerID+"."+daemonLifecyclePauseAfterCreateAdmission+".json")
	if err := daemonLifecycleWaitFile(ctx, signalPath, 30*time.Second); err != nil {
		return nil, err
	}
	r.recordEvent("crashed daemon after create admission", 0)
	if err := (*stopDaemon)(); err != nil {
		return nil, err
	}
	r.admissionPauseDir = ""
	r.admissionPausePoint = ""
	nextStopDaemon, err = r.startDaemon(ctx, "after-create-admission-restart")
	if err != nil {
		return nil, err
	}
	*stopDaemon = nextStopDaemon
	if err := r.waitTerminal(ctx, c); err != nil {
		return nil, err
	}
	if c.FinalState != daemonLifecycleStateFailed || c.ErrorCode != daemonLifecycleErrorRuntimeStartFailed {
		return nil, fmt.Errorf("%s final state=%s error=%s, want failed/%s", c.Name, c.FinalState, c.ErrorCode, daemonLifecycleErrorRuntimeStartFailed)
	}
	return c, nil
}

func (r *daemonLifecycleRun) runStartAdmissionCrashCase(ctx context.Context, stopDaemon *func() error) (*daemonLifecycleCase, error) {
	c, err := r.createCase(ctx, "daemon-restart-after-start-admission", []string{"/bin/sh", "-c", "echo start-admission-recovered"})
	if err != nil {
		return nil, err
	}
	if err := r.waitState(ctx, c, daemonLifecycleStateCreated); err != nil {
		return nil, err
	}
	c.CreateOperationID = c.OperationID
	if err := (*stopDaemon)(); err != nil {
		return nil, err
	}
	r.admissionPauseDir = filepath.Join(r.evidenceDir, "daemon-admission-pause")
	r.admissionPausePoint = daemonLifecyclePauseAfterStartAdmission
	nextStopDaemon, err := r.startDaemon(ctx, "pause-after-start-admission")
	if err != nil {
		return nil, err
	}
	*stopDaemon = nextStopDaemon
	if err := r.postJSONThenDisconnect(ctx, "/v1/containers/"+c.ContainerID+"/start", map[string]string{}, filepath.Join(r.evidenceDir, c.Name, "start-request.txt")); err != nil {
		return nil, err
	}
	if err := r.waitState(ctx, c, daemonLifecycleStateStarting); err != nil {
		return nil, err
	}
	c.StartOperationID = c.OperationID
	signalPath := filepath.Join(r.admissionPauseDir, c.ContainerID+"."+daemonLifecyclePauseAfterStartAdmission+".json")
	if err := daemonLifecycleWaitFile(ctx, signalPath, 30*time.Second); err != nil {
		return nil, err
	}
	r.recordEvent("crashed daemon after start admission", 0)
	if err := (*stopDaemon)(); err != nil {
		return nil, err
	}
	r.admissionPauseDir = ""
	r.admissionPausePoint = ""
	nextStopDaemon, err = r.startDaemon(ctx, "after-start-admission-restart")
	if err != nil {
		return nil, err
	}
	*stopDaemon = nextStopDaemon
	if err := r.waitTerminal(ctx, c); err != nil {
		return nil, err
	}
	if err := r.readLogs(ctx, c); err != nil {
		return nil, err
	}
	if err := daemonLifecycleRequireExit(c, daemonLifecycleStateExited, 0, ""); err != nil {
		return nil, err
	}
	if err := daemonLifecycleRequireLogContains(c.StdoutPath, "start-admission-recovered"); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *daemonLifecycleRun) newCaseFromRunResponse(name string, response runResponse) *daemonLifecycleCase {
	return r.newCaseFromContainerRecord(name, containerRecord{
		ID:          response.ID,
		OperationID: response.OperationID,
		ImageDigest: response.ImageDigest,
		State:       response.State,
	})
}

func (r *daemonLifecycleRun) newCaseFromContainerRecord(name string, container containerRecord) *daemonLifecycleCase {
	return &daemonLifecycleCase{
		Name:        name,
		OperationID: container.OperationID,
		ContainerID: container.ID,
		FinalState:  container.State,
		ExitCode:    container.ExitCode,
		ErrorCode:   container.ErrorCode,
		Artifacts: map[string]string{
			"bundle":        filepath.Join(r.roots["bundles"], container.ID),
			"runtime":       filepath.Join(r.roots["runtime"], container.ID),
			"runtimeStdout": filepath.Join(r.roots["runtime"], "logs", container.ID, string(chamberRuntime.StdoutLogStream)+".log"),
			"runtimeStderr": filepath.Join(r.roots["runtime"], "logs", container.ID, string(chamberRuntime.StderrLogStream)+".log"),
			"supervisor":    filepath.Join(r.roots["supervisors"], container.ID),
		},
	}
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
	deadline := time.NewTimer(daemonLifecycleContainerTimeout)
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
		if container.OperationID != "" {
			c.OperationID = container.OperationID
		}
		c.ExitCode = container.ExitCode
		c.ErrorCode = container.ErrorCode
		if container.State == daemonLifecycleStateRunning {
			if err := r.pollLiveLogs(ctx, c); err != nil {
				return err
			}
		}
		if list, err := r.listContainers(ctx); err == nil {
			c.ListPolls = append(c.ListPolls, list)
			_ = daemonLifecycleWriteJSON(filepath.Join(caseDir, "list-polls.json"), c.ListPolls)
		}
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

func (r *daemonLifecycleRun) pollLiveLogs(ctx context.Context, c *daemonLifecycleCase) error {
	caseDir := filepath.Join(r.evidenceDir, c.Name)
	for _, stream := range []string{"stdout", "stderr"} {
		body, err := daemonLifecycleGetBytes(ctx, r.daemonURL+"/v1/containers/"+c.ContainerID+"/logs?stream="+stream, "")
		if err != nil {
			return fmt.Errorf("read live %s for %s: %w", stream, c.Name, err)
		}
		c.LogPolls = append(c.LogPolls, daemonLifecycleLogPoll{At: time.Now().UTC(), Stream: stream, Bytes: len(body)})
	}
	return daemonLifecycleWriteJSON(filepath.Join(caseDir, "log-polls.json"), c.LogPolls)
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

func (r *daemonLifecycleRun) decommissionContainer(ctx context.Context, c *daemonLifecycleCase) error {
	_, err := daemonLifecyclePostJSON(ctx, r.daemonURL+"/v1/containers/"+c.ContainerID+"/decommission", map[string]string{}, filepath.Join(r.evidenceDir, c.Name, "decommission.json"))
	return err
}

func (r *daemonLifecycleRun) stopContainer(ctx context.Context, c *daemonLifecycleCase) error {
	_, err := daemonLifecyclePostJSON(ctx, r.daemonURL+"/v1/containers/"+c.ContainerID+"/stop", map[string]string{}, filepath.Join(r.evidenceDir, c.Name, "stop.json"))
	return err
}

func (r *daemonLifecycleRun) deleteContainer(ctx context.Context, c *daemonLifecycleCase) error {
	if _, err := daemonLifecycleDoJSON(ctx, http.MethodDelete, r.daemonURL+"/v1/containers/"+c.ContainerID, nil, filepath.Join(r.evidenceDir, c.Name, "delete.json")); err != nil {
		return fmt.Errorf("delete container %s: %w", c.ContainerID, err)
	}
	if _, err := daemonLifecycleGetBytes(ctx, r.daemonURL+"/v1/containers/"+c.ContainerID, ""); err == nil {
		return fmt.Errorf("container %s still exists after delete", c.ContainerID)
	}
	return nil
}

func (r *daemonLifecycleRun) requireStartRejected(ctx context.Context, c *daemonLifecycleCase) error {
	_, err := daemonLifecyclePostJSON(ctx, r.daemonURL+"/v1/containers/"+c.ContainerID+"/start", map[string]string{}, filepath.Join(r.evidenceDir, c.Name, "restart-rejected.json"))
	if err == nil {
		return fmt.Errorf("start decommissioned container %s unexpectedly succeeded", c.ContainerID)
	}
	if !strings.Contains(err.Error(), "HTTP 409") {
		return fmt.Errorf("start decommissioned container %s: got %w, want HTTP 409", c.ContainerID, err)
	}
	return nil
}

func (r *daemonLifecycleRun) listContainers(ctx context.Context) ([]containerRecord, error) {
	body, err := daemonLifecycleGetBytes(ctx, r.daemonURL+"/v1/containers", "")
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	var response listContainersResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode container list: %w", err)
	}
	return response.Containers, nil
}

func (r *daemonLifecycleRun) auditDurableOperations(ctx context.Context) error {
	body, err := daemonLifecycleGetBytes(ctx, r.daemonURL+"/v1/operations", "")
	if err != nil {
		return fmt.Errorf("list operations: %w", err)
	}
	if err := os.WriteFile(filepath.Join(r.evidenceDir, "operations-final.json"), body, 0o600); err != nil {
		return fmt.Errorf("write final operation evidence: %w", err)
	}
	var response daemonLifecycleOperationsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("decode operation list: %w", err)
	}
	if response.PendingCleanups != 0 {
		return fmt.Errorf("pending cleanup records = %d, want 0", response.PendingCleanups)
	}
	for _, operation := range response.Operations {
		if operation.State == "running" {
			return fmt.Errorf("operation %s (%s) remained running after lifecycle cleanup", operation.ID, operation.Kind)
		}
	}
	return nil
}

func (r *daemonLifecycleRun) listContainerIDs(ctx context.Context) (map[string]bool, error) {
	containers, err := r.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(containers))
	for _, container := range containers {
		ids[container.ID] = true
	}
	return ids, nil
}

func (r *daemonLifecycleRun) waitForNewContainer(ctx context.Context, before map[string]bool) (containerRecord, error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()

	for {
		containers, err := r.listContainers(ctx)
		if err == nil {
			for _, container := range containers {
				if !before[container.ID] {
					return container, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return containerRecord{}, fmt.Errorf("wait for disconnected run container: %w", ctx.Err())
		case <-deadline.C:
			return containerRecord{}, fmt.Errorf("disconnected run did not create a container before timeout")
		case <-ticker.C:
		}
	}
}

func (r *daemonLifecycleRun) waitSupervisorCompleted(ctx context.Context, c *daemonLifecycleCase, timeout time.Duration) error {
	supervisorPath := filepath.Join(c.Artifacts["supervisor"], "supervisor.json")
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		body, err := os.ReadFile(supervisorPath)
		if err == nil {
			var state struct {
				Phase string `json:"phase"`
			}
			if err := json.Unmarshal(body, &state); err != nil {
				return fmt.Errorf("decode supervisor file: %w", err)
			}
			if state.Phase == daemonLifecycleSupervisorPhaseCompleted {
				evidencePath := filepath.Join(r.evidenceDir, c.Name, "supervisor-completed.json")
				if err := os.WriteFile(evidencePath, body, 0o600); err != nil {
					return fmt.Errorf("write supervisor completed evidence: %w", err)
				}
				return nil
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read supervisor file: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for completed supervisor file: %w", ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("supervisor file for %s did not reach completed phase before timeout", c.Name)
		case <-ticker.C:
		}
	}
}

func (r *daemonLifecycleRun) postRunThenDisconnect(ctx context.Context, command []string, evidencePath string) error {
	return r.postJSONThenDisconnect(ctx, "/v1/containers/run", map[string]any{
		"image": r.image, "command": command,
	}, evidencePath)
}

func (r *daemonLifecycleRun) postJSONThenDisconnect(ctx context.Context, path string, request any, evidencePath string) error {
	parsed, err := url.Parse(r.daemonURL)
	if err != nil {
		return fmt.Errorf("parse daemon URL: %w", err)
	}
	if parsed.Scheme != "http" || parsed.Host == "" {
		return fmt.Errorf("disconnected run proof requires HTTP daemon URL, got %q", r.daemonURL)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	requestHead := fmt.Sprintf(
		"POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		path,
		parsed.Host,
		len(payload),
	)
	if err := daemonLifecycleWriteRawRequest(evidencePath, requestHead, payload); err != nil {
		return err
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", parsed.Host)
	if err != nil {
		return fmt.Errorf("dial daemon: %w", err)
	}
	if _, err := conn.Write([]byte(requestHead)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("write disconnected run request head: %w", err)
	}
	if _, err := conn.Write(payload); err != nil {
		_ = conn.Close()
		return fmt.Errorf("write disconnected run request body: %w", err)
	}
	return conn.Close()
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

func daemonLifecycleVerifyDecommissioned(c *daemonLifecycleCase) error {
	c.Decommission = make(map[string]bool)
	for _, name := range []string{"bundle", "runtime"} {
		path := c.Artifacts[name]
		_, err := os.Stat(path)
		removed := os.IsNotExist(err)
		c.Decommission[name] = removed
		if !removed {
			return fmt.Errorf("%s artifact %s remains at %s (stat error: %v)", c.Name, name, path, err)
		}
	}
	for _, name := range []string{"runtimeStdout", "runtimeStderr", "supervisor"} {
		path := c.Artifacts[name]
		_, err := os.Stat(path)
		retained := err == nil
		c.Decommission[name] = !retained
		if !retained {
			return fmt.Errorf("%s evidence %s missing at %s (stat error: %v)", c.Name, name, path, err)
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

func daemonLifecycleWaitFile(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %s: %w", path, ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("wait for %s timed out", path)
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

func daemonLifecycleWriteRawRequest(path string, head string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	content := append([]byte(head), body...)
	content = append(content, '\n')
	return os.WriteFile(path, content, 0600)
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
