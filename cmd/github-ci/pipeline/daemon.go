package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
)

const defaultPollInterval = time.Second

type httpRecord struct {
	Method     string          `json:"method"`
	URL        string          `json:"url"`
	StatusCode int             `json:"status_code"`
	Body       json.RawMessage `json:"body,omitempty"`
	RawBody    string          `json:"raw_body,omitempty"`
}

type runResponse struct {
	OperationID string `json:"operation_id"`
	ID          string `json:"id"`
	ImageDigest string `json:"image_digest"`
	State       string `json:"state"`
}

type containerRecord struct {
	ID          string    `json:"id"`
	OperationID string    `json:"operation_id"`
	Image       string    `json:"image"`
	ImageDigest string    `json:"image_digest"`
	Runtime     string    `json:"runtime"`
	State       string    `json:"state"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	ErrorCode   string    `json:"error_code,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

type listContainersResponse struct {
	Containers []containerRecord `json:"containers"`
}

type pollRecord struct {
	At        time.Time        `json:"at"`
	Container *containerRecord `json:"container,omitempty"`
	Response  httpRecord       `json:"response"`
}

type proofRecord struct {
	Passed      bool              `json:"passed"`
	StartedAt   time.Time         `json:"started_at"`
	FinishedAt  time.Time         `json:"finished_at"`
	DaemonURL   string            `json:"daemon_url"`
	Image       string            `json:"image"`
	Repo        string            `json:"repo"`
	TestCommand string            `json:"test_command"`
	OperationID string            `json:"operation_id"`
	ContainerID string            `json:"container_id"`
	FinalState  string            `json:"final_state"`
	ExitCode    *int              `json:"exit_code,omitempty"`
	ErrorCode   string            `json:"error_code,omitempty"`
	EvidenceDir string            `json:"evidence_dir"`
	Git         map[string]string `json:"git"`
	Host        map[string]string `json:"host"`
	Failure     string            `json:"failure,omitempty"`
}

func RunDaemonSupervised(ctx context.Context, cfg Config) (int, error) {
	ctx, cancel, cfg, err := prepareRun(ctx, cfg)
	if err != nil {
		return 1, err
	}
	defer cancel()

	workspace, err := workspacePath(cfg)
	if err != nil {
		return 1, err
	}
	daemonURL := strings.TrimRight(strings.TrimSpace(cfg.DaemonURL), "/")
	if daemonURL == "" {
		return 1, fmt.Errorf("daemon URL is required for %s pipeline", DaemonSupervised)
	}
	evidenceDir, cleanup, err := prepareEvidenceDir(ctx, cfg)
	if err != nil {
		return 1, err
	}
	defer cleanup()

	startedAt := time.Now().UTC()
	proof := proofRecord{
		StartedAt:   startedAt,
		DaemonURL:   daemonURL,
		Image:       cfg.Image,
		Repo:        workspace,
		TestCommand: GoTestShellCommand,
		EvidenceDir: evidenceDir,
		Git:         gitEvidence(workspace),
		Host:        hostEvidence(),
	}
	defer func() {
		proof.FinishedAt = time.Now().UTC()
		_ = writeJSON(filepath.Join(evidenceDir, "proof.json"), proof)
	}()
	if err := writeJSON(filepath.Join(evidenceDir, "input.json"), proof); err != nil {
		return 1, err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	if _, err := getJSON(ctx, client, daemonURL+"/healthz", filepath.Join(evidenceDir, "healthz.json")); err != nil {
		proof.Failure = err.Error()
		return 1, err
	}
	if _, err := postJSON(ctx, client, daemonURL+"/v1/images/pull", map[string]string{
		"reference": cfg.Image,
	}, filepath.Join(evidenceDir, "pull.json")); err != nil {
		proof.Failure = err.Error()
		return 1, err
	}

	runExchange, err := postJSON(ctx, client, daemonURL+"/v1/containers/run", map[string]any{
		"image":   cfg.Image,
		"command": []string{"/bin/sh", "-c", "cd /workspace && " + GoTestShellCommand},
		"mounts": []chamberBundle.Mount{{
			Type:    "bind",
			Source:  workspace,
			Target:  "/workspace",
			Options: []string{"rbind", "ro"},
		}},
	}, filepath.Join(evidenceDir, "run.json"))
	if err != nil {
		proof.Failure = err.Error()
		return 1, err
	}
	var runResult runResponse
	if err := json.Unmarshal(runExchange.Body, &runResult); err != nil {
		proof.Failure = err.Error()
		return 1, fmt.Errorf("decode daemon run response: %w", err)
	}
	proof.OperationID = runResult.OperationID
	proof.ContainerID = runResult.ID

	finalContainer, polls, err := waitForDaemonContainer(ctx, client, daemonURL, runResult.ID)
	if writeErr := writeJSON(filepath.Join(evidenceDir, "polls.json"), polls); writeErr != nil && err == nil {
		err = writeErr
	}
	if finalContainer != nil {
		proof.FinalState = finalContainer.State
		proof.ExitCode = finalContainer.ExitCode
		proof.ErrorCode = finalContainer.ErrorCode
	}
	if err != nil {
		proof.Failure = err.Error()
		return 1, err
	}

	stdout, stdoutErr := getBytes(ctx, client, daemonURL+"/v1/containers/"+runResult.ID+"/logs?stream=stdout")
	if writeErr := writeLog(filepath.Join(evidenceDir, "stdout.log"), stdout, cfg.Stdout); writeErr != nil && stdoutErr == nil {
		stdoutErr = writeErr
	}
	stderr, stderrErr := getBytes(ctx, client, daemonURL+"/v1/containers/"+runResult.ID+"/logs?stream=stderr")
	if writeErr := writeLog(filepath.Join(evidenceDir, "stderr.log"), stderr, cfg.Stderr); writeErr != nil && stderrErr == nil {
		stderrErr = writeErr
	}
	if stdoutErr != nil || stderrErr != nil {
		err := errors.Join(stdoutErr, stderrErr)
		proof.Failure = err.Error()
		return 1, err
	}

	exitCode := 1
	if finalContainer.ExitCode != nil {
		exitCode = *finalContainer.ExitCode
	}
	proof.Passed = finalContainer.State == "exited" && exitCode == 0
	if !proof.Passed {
		err := fmt.Errorf("daemon-supervised CI finished state=%s exit_code=%d error_code=%s", finalContainer.State, exitCode, finalContainer.ErrorCode)
		proof.Failure = err.Error()
		if finalContainer.State == "exited" {
			return exitCode, nil
		}
		return exitCode, err
	}
	return 0, nil
}

func prepareEvidenceDir(ctx context.Context, cfg Config) (string, func(), error) {
	if strings.TrimSpace(cfg.EvidenceDir) != "" {
		path, err := filepath.Abs(cfg.EvidenceDir)
		if err != nil {
			return "", func() {}, fmt.Errorf("resolve evidence dir: %w", err)
		}
		if err := os.MkdirAll(path, 0700); err != nil {
			return "", func() {}, fmt.Errorf("create evidence dir: %w", err)
		}
		return path, func() {}, nil
	}
	root, cleanup, err := createRunRoot(ctx, Config{Root: cfg.Root, Keep: true})
	if err != nil {
		return "", cleanup, err
	}
	return root, cleanup, nil
}

func waitForDaemonContainer(ctx context.Context, client *http.Client, daemonURL string, containerID string) (*containerRecord, []pollRecord, error) {
	ticker := time.NewTicker(defaultPollInterval)
	defer ticker.Stop()

	var polls []pollRecord
	for {
		container, poll, err := pollDaemonContainer(ctx, client, daemonURL, containerID)
		if err != nil {
			return container, polls, err
		}
		polls = append(polls, poll)
		if container != nil && (container.State == "exited" || container.State == "failed") {
			return container, polls, nil
		}

		select {
		case <-ctx.Done():
			return container, polls, ctx.Err()
		case <-ticker.C:
		}
	}
}

func pollDaemonContainer(ctx context.Context, client *http.Client, daemonURL string, containerID string) (*containerRecord, pollRecord, error) {
	exchange, err := getJSON(ctx, client, daemonURL+"/v1/containers", "")
	poll := pollRecord{
		At:       time.Now().UTC(),
		Response: exchange,
	}
	if err != nil {
		return nil, poll, err
	}
	var list listContainersResponse
	if err := json.Unmarshal(exchange.Body, &list); err != nil {
		return nil, poll, fmt.Errorf("decode container list: %w", err)
	}
	for _, container := range list.Containers {
		if container.ID == containerID {
			current := container
			poll.Container = &current
			return &current, poll, nil
		}
	}
	return nil, poll, nil
}

func getJSON(ctx context.Context, client *http.Client, url string, evidencePath string) (httpRecord, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return httpRecord{}, err
	}
	return doJSON(client, request, evidencePath)
}

func postJSON(ctx context.Context, client *http.Client, url string, body any, evidencePath string) (httpRecord, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return httpRecord{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return httpRecord{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	return doJSON(client, request, evidencePath)
}

func doJSON(client *http.Client, request *http.Request, evidencePath string) (httpRecord, error) {
	response, err := client.Do(request)
	if err != nil {
		return httpRecord{}, err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return httpRecord{}, err
	}
	record := httpRecord{
		Method:     request.Method,
		URL:        request.URL.String(),
		StatusCode: response.StatusCode,
	}
	if json.Valid(body) {
		record.Body = append(json.RawMessage(nil), body...)
	} else {
		record.RawBody = string(body)
	}
	if evidencePath != "" {
		if err := writeJSON(evidencePath, record); err != nil {
			return record, err
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return record, fmt.Errorf("%s %s returned HTTP %d: %s", request.Method, request.URL.String(), response.StatusCode, strings.TrimSpace(string(body)))
	}
	return record, nil
}

func getBytes(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return body, fmt.Errorf("GET %s returned HTTP %d: %s", url, response.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func writeLog(path string, content []byte, writers []io.Writer) error {
	if err := os.WriteFile(path, content, 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	for _, writer := range writers {
		if writer == nil {
			continue
		}
		if _, err := writer.Write(content); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func gitEvidence(repo string) map[string]string {
	evidence := map[string]string{
		"head":   commandOutput("git", "-C", repo, "rev-parse", "HEAD"),
		"status": commandOutput("git", "-C", repo, "status", "--short"),
	}
	digest, err := gitSnapshotSHA256(repo)
	if err != nil {
		evidence["source_snapshot_error"] = err.Error()
	} else {
		evidence["source_snapshot_sha256"] = digest
	}
	return evidence
}

// gitSnapshotSHA256 identifies the exact committed base plus tracked and
// untracked worktree content used by a dogfood build. This keeps dirty-tree
// proof auditable without copying the full source diff into every run record.
func gitSnapshotSHA256(repo string) (string, error) {
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("read git HEAD: %w", err)
	}
	diff, err := exec.Command("git", "-C", repo, "diff", "--binary", "HEAD", "--").Output()
	if err != nil {
		return "", fmt.Errorf("read tracked git diff: %w", err)
	}
	untracked, err := exec.Command("git", "-C", repo, "ls-files", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		return "", fmt.Errorf("list untracked git files: %w", err)
	}

	digest := sha256.New()
	_, _ = digest.Write([]byte("head\x00"))
	_, _ = digest.Write(bytes.TrimSpace(head))
	_, _ = digest.Write([]byte("\x00tracked-diff\x00"))
	_, _ = digest.Write(diff)
	for _, rawPath := range bytes.Split(untracked, []byte{0}) {
		if len(rawPath) == 0 {
			continue
		}
		relativePath := filepath.Clean(string(rawPath))
		if filepath.IsAbs(relativePath) || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("git returned unsafe untracked path %q", relativePath)
		}
		path := filepath.Join(repo, relativePath)
		info, err := os.Lstat(path)
		if err != nil {
			return "", fmt.Errorf("inspect untracked file %q: %w", relativePath, err)
		}
		_, _ = fmt.Fprintf(digest, "\x00untracked\x00%s\x00%o\x00", relativePath, info.Mode())
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return "", fmt.Errorf("read untracked symlink %q: %w", relativePath, err)
			}
			_, _ = digest.Write([]byte(target))
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open untracked file %q: %w", relativePath, err)
		}
		_, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return "", fmt.Errorf("hash untracked file %q: %w", relativePath, errors.Join(copyErr, closeErr))
		}
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return "", errors.Join(copyErr, closeErr)
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func hostEvidence() map[string]string {
	hostname, _ := os.Hostname()
	executable, _ := os.Executable()
	return map[string]string{
		"hostname":   hostname,
		"kernel":     commandOutput("uname", "-a"),
		"executable": executable,
	}
}

func commandOutput(name string, args ...string) string {
	output, err := exec.Command(name, args...).CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if text == "" {
			return err.Error()
		}
		return text + "\n" + err.Error()
	}
	return text
}
