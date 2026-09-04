package pipeline

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestGitSnapshotSHA256IdentifiesCurrentSourceSnapshot(t *testing.T) {
	digest, err := gitSnapshotSHA256(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("gitSnapshotSHA256() error = %v", err)
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("git snapshot digest = %q, decode error = %v", digest, err)
	}
}

func TestRequireSourceSnapshotRejectsWorktreeChange(t *testing.T) {
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	runGit("init", "--quiet")
	path := filepath.Join(repo, "source.go")
	if err := os.WriteFile(path, []byte("package source\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(initial) error = %v", err)
	}
	runGit("add", "source.go")
	runGit("-c", "user.name=Chamber Test", "-c", "user.email=chamber@example.invalid", "commit", "--quiet", "-m", "initial")
	digest, err := gitSnapshotSHA256(repo)
	if err != nil {
		t.Fatalf("gitSnapshotSHA256() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("package changed\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(changed) error = %v", err)
	}
	if err := requireSourceSnapshot(repo, digest); err == nil {
		t.Fatal("requireSourceSnapshot() error = nil, want changed-source failure")
	}
}

func TestRunDaemonSupervisedWritesProofAndLogs(t *testing.T) {
	workspace := t.TempDir()
	root := t.TempDir()
	evidenceDir := filepath.Join(t.TempDir(), "proof")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var runRequest struct {
		Image   string   `json:"image"`
		Command []string `json:"command"`
		Mounts  []struct {
			Source  string   `json:"source"`
			Target  string   `json:"target"`
			Options []string `json:"options"`
		} `json:"mounts"`
	}

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			writeTestJSON(t, w, map[string]string{"status": "ok"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/images/pull":
			writeTestJSON(t, w, map[string]string{"digest": "sha256:image"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/containers/run":
			if err := json.NewDecoder(r.Body).Decode(&runRequest); err != nil {
				t.Fatalf("decode run request: %v", err)
			}
			writeTestJSON(t, w, map[string]string{
				"operation_id": "operation-1",
				"id":           "container-1",
				"image_digest": "sha256:image",
				"state":        "starting",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/containers":
			writeTestJSON(t, w, map[string]any{
				"containers": []map[string]any{{
					"id":           "container-1",
					"operation_id": "operation-1",
					"image":        DefaultImage,
					"image_digest": "sha256:image",
					"runtime":      "runc",
					"state":        "exited",
					"exit_code":    0,
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/containers/container-1/logs" && r.URL.Query().Get("stream") == "stdout":
			_, _ = w.Write([]byte("ok stdout\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/containers/container-1/logs" && r.URL.Query().Get("stream") == "stderr":
			_, _ = w.Write([]byte("ok stderr\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()

	exitCode, err := RunDaemonSupervised(context.Background(), Config{
		Root:        root,
		Workdir:     workspace,
		DaemonURL:   daemon.URL,
		EvidenceDir: evidenceDir,
		Timeout:     time.Minute,
		Stdout:      []io.Writer{&stdout},
		Stderr:      []io.Writer{&stderr},
	})
	if err != nil {
		t.Fatalf("RunDaemonSupervised() error = %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	if runRequest.Image != DefaultImage {
		t.Fatalf("run image = %q, want default image", runRequest.Image)
	}
	if len(runRequest.Mounts) != 1 || runRequest.Mounts[0].Source != workspace || runRequest.Mounts[0].Target != "/workspace" {
		t.Fatalf("mounts = %#v, want workspace mount", runRequest.Mounts)
	}
	if stdout.String() != "ok stdout\n" || stderr.String() != "ok stderr\n" {
		t.Fatalf("stdout=%q stderr=%q, want daemon logs copied", stdout.String(), stderr.String())
	}
	proof, err := os.ReadFile(filepath.Join(evidenceDir, "proof.json"))
	if err != nil {
		t.Fatalf("read proof: %v", err)
	}
	if !bytes.Contains(proof, []byte(`"passed": true`)) {
		t.Fatalf("proof = %s, want passed true", proof)
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("write JSON response: %v", err)
	}
}
