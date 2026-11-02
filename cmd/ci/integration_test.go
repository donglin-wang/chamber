package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
	"github.com/donglin-wang/chamber/pkg/shared/subprocess"
)

func requireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("CHAMBER_INTEGRATION") != "1" {
		t.Skip("set CHAMBER_INTEGRATION=1 on rootless-capable Linux")
	}
}

func gitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var output strings.Builder
	if err := runCommand(ctx, dir, &output, "git", args...); err != nil {
		t.Fatalf("git: %v\n%s", err, output.String())
	}
	return strings.TrimSpace(output.String())
}

func commitFixture(t *testing.T, root string) string {
	t.Helper()
	gitCommand(t, root, "init", "-q", "-b", "main")
	gitCommand(t, root, "add", ".")
	gitCommand(t, root, "-c", "user.name=CI test", "-c", "user.email=ci@example.test", "commit", "-qm", "fixture")
	return gitCommand(t, root, "rev-parse", "HEAD")
}

func TestSelfUpdateIntegration(t *testing.T) {
	requireIntegration(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	workspace := t.TempDir()
	project, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// A local source repo contains this exact implementation, including uncommitted changes.
	source := filepath.Join(workspace, "source")
	if err := os.MkdirAll(filepath.Join(source, "cmd", "ci"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(filepath.Join(project, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.CopyFS(filepath.Join(source, "pkg"), os.DirFS(filepath.Join(project, "pkg"))); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "cmd", "ci", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	sha := commitFixture(t, source)
	bin := filepath.Join(workspace, "ci")
	var buildOutput strings.Builder
	if err := runCommand(ctx, project, &buildOutput, "go", "build", "-o", bin, "./cmd/ci"); err != nil {
		t.Fatalf("build: %v\n%s", err, buildOutput.String())
	}
	// Rewrite a configured HTTPS Git remote to the local fixture, without a network
	// service, changing user Git config, or weakening production URL validation.
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrappers := filepath.Join(workspace, "bin")
	if err := os.Mkdir(wrappers, 0700); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	wrapper := "#!/bin/sh\nexec " + quote(gitPath) + " -c " + quote("url."+source+".insteadOf=https://update.example.test/chamber.git") + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrappers, "git"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()
	root := filepath.Join(workspace, "state")
	seeded := seedSettledJob(t, root)
	configPath := filepath.Join(workspace, "config.json")
	settings := map[string]any{"root": root, "listen": address, "public_url": "http://" + address,
		"repositories": map[string]any{"test": map[string]string{"name": "owner/repo", "token_env": "CI_INTEGRATION_TOKEN", "secret_env": "CI_INTEGRATION_SECRET"}},
		"update":       map[string]string{"clone_url": "https://update.example.test/chamber.git", "ref": "refs/heads/main"}}
	configData, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatal(err)
	}
	serverLog, err := os.Create(filepath.Join(workspace, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer serverLog.Close()
	command := subprocess.CommandContext(ctx, bin, "-config", configPath)
	command.Env = append(os.Environ(), "CI_INTEGRATION_TOKEN=unused", "CI_INTEGRATION_SECRET=unused", "PATH="+wrappers+":"+os.Getenv("PATH"))
	command.Stdout, command.Stderr = serverLog, serverLog
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		_ = command.Wait()
		if t.Failed() {
			data, _ := os.ReadFile(serverLog.Name())
			t.Log(string(data))
			data, _ = os.ReadFile(filepath.Join(root, "update.log"))
			t.Log(string(data))
		}
	}()
	client := &http.Client{Timeout: time.Second}
	waitRevision := func(want string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(serverLog.Name())
			if strings.Contains(string(data), "revision="+want+" ") {
				resp, err := client.Get("http://" + address + "/runs/missing")
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == 404 {
						return
					}
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("server did not report revision %s", want)
	}
	waitRevision("development")
	// Starting another copy must fail before it can recover this server's jobs.
	duplicate := subprocess.CommandContext(ctx, bin, "-config", configPath)
	duplicate.Env = command.Env
	if output, err := duplicate.CombinedOutput(); err == nil || !strings.Contains(string(output), "state directory is already in use") {
		t.Fatalf("second server was not rejected by the state lock: %v\n%s", err, output)
	}
	passcode, err := readPasscode(root)
	if err != nil {
		t.Fatal(err)
	}
	postUpdate := func() {
		t.Helper()
		request, _ := http.NewRequest("POST", "http://"+address+"/update", nil)
		request.Header.Set("Authorization", "Bearer "+passcode)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 202 {
			data, _ := io.ReadAll(response.Body)
			t.Fatalf("update: %d %s", response.StatusCode, data)
		}
	}
	// Check that the port never becomes available to another server during exec.
	// Accepted HTTP connections may still close during server shutdown.
	probeCtx, stopProbes := context.WithCancel(ctx)
	probeDone := make(chan struct{})
	probeErrors := make(chan error, 1)
	probeClient := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	go func() {
		defer close(probeDone)
		for probeCtx.Err() == nil {
			competing, err := net.Listen("tcp", address)
			if err == nil {
				competing.Close()
				probeErrors <- fmt.Errorf("CI released its listening port")
				return
			}
			if !errors.Is(err, syscall.EADDRINUSE) {
				probeErrors <- err
				return
			}
			response, err := probeClient.Get("http://" + address + "/health")
			if err == nil {
				response.Body.Close()
				if response.StatusCode != 200 {
					err = fmt.Errorf("health returned %d", response.StatusCode)
				}
			}
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
				select {
				case probeErrors <- err:
				default:
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	defer func() { stopProbes(); <-probeDone; probeClient.CloseIdleConnections() }()
	postUpdate()
	waitRevision(sha)
	after, err := readPasscode(root)
	if err != nil || after != passcode {
		t.Fatalf("passcode changed after exec: %v", err)
	}
	if err := command.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("original process PID was lost", err)
	}
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		lock.Close()
		t.Fatal("updated server did not reacquire the state lock")
	}
	lock.Close()
	if _, err := os.Stat(bin + ".previous"); err != nil {
		t.Fatal("previous binary not retained", err)
	}
	// The original passcode must also authorize a second update.
	postUpdate()
	eventually(t, func() bool {
		entries, _ := os.ReadDir(root)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "update-source-") {
				return true
			}
		}
		return false
	})
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(serverLog.Name())
		if strings.Count(string(data), "CI listening") >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	data, _ := os.ReadFile(serverLog.Name())
	if strings.Count(string(data), "CI listening") < 3 {
		t.Fatal("second update did not complete")
	}
	after, err = readPasscode(root)
	if err != nil || after != passcode {
		t.Fatal("passcode changed on second update")
	}
	reopened, err := openSQLiteStore(filepath.Join(root, "jobs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	preserved, err := reopened.Get(ctx, seeded.ID)
	reopened.Close()
	if err != nil || preserved.State != "success" || !preserved.Reported || !preserved.Cleaned || preserved.Version != seeded.Version {
		t.Fatalf("update changed persisted job: %+v %v", preserved, err)
	}
	// A compilable candidate still must parse config and answer health. An
	// invalid edited config rejects activation while the current server survives.
	if err := os.WriteFile(configPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	postUpdate()
	deadline = time.Now().Add(time.Minute)
	rejected := false
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(serverLog.Name())
		if strings.Contains(string(data), "candidate did not start its health endpoint") {
			rejected = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !rejected {
		t.Fatal("candidate with invalid config was not rejected")
	}
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get("http://" + address + "/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(body) != sha {
		t.Fatalf("current server did not survive rejected update: %d %s", response.StatusCode, body)
	}
	stopProbes()
	<-probeDone
	select {
	case err := <-probeErrors:
		t.Fatal("listener unavailable during update", err)
	default:
	}
	t.Logf("two HTTP-triggered updates retained PID %d, address %s, retained state lock and first-start passcode", command.Process.Pid, address)
}

func TestDockerfileExecutionIntegration(t *testing.T) {
	requireIntegration(t)
	for _, exitCode := range []int{0, 7} {
		t.Run(fmt.Sprint(exitCode), func(t *testing.T) {
			s := testServer(t)
			source := t.TempDir()
			dockerfile := fmt.Sprintf("FROM busybox:1.37\nCMD [\"sh\",\"-c\",\"echo chamber-ci-stdout; echo chamber-ci-stderr >&2; exit %d\"]\n", exitCode)
			if err := os.WriteFile(filepath.Join(source, "Dockerfile.chamber"), []byte(dockerfile), 0600); err != nil {
				t.Fatal(err)
			}
			sha := commitFixture(t, source)
			r := s.config.Repositories["test"]
			r.CloneURL = source
			s.config.Repositories["test"] = r
			// Prefer the test host's tools, but managed defaults also work.
			buildctl, _ := exec.LookPath("buildctl")
			buildkitd, _ := exec.LookPath("buildkitd")
			rootlesskit, _ := exec.LookPath("rootlesskit")
			runc, _ := exec.LookPath("runc")
			if buildctl != "" && buildkitd != "" && rootlesskit != "" && runc != "" {
				s.config.BuildKit = chamberImage.BuildKitConfig{BuildctlPath: buildctl, BuildkitdPath: buildkitd, RootlessKitPath: rootlesskit, RuncPath: runc}
				// The runtime factory makes a configured executable private; give it
				// a user-owned copy rather than a system-owned tool.
				data, err := os.ReadFile(runc)
				if err != nil {
					t.Fatal(err)
				}
				s.config.RuntimePath = filepath.Join(s.config.Root, "runc")
				if err := os.WriteFile(s.config.RuntimePath, data, 0700); err != nil {
					t.Fatal(err)
				}
			}
			j := job{ID: jobID("test", sha), Repository: "test", SHA: sha, Ref: "refs/heads/main"}
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
				defer cleanupCancel()
				if err := cleanupJob(cleanupCtx, s.config, j); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			logPath := filepath.Join(s.config.Root, "execution.log")
			output, err := openLog(logPath, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			state, err := executeJob(ctx, s.config, j, output)
			if closeErr := output.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			data, readErr := os.ReadFile(logPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := "success"
			if exitCode != 0 {
				want = "failure"
			}
			if state != want || (exitCode == 0 && err != nil) || !strings.Contains(string(data), "chamber-ci-stdout") || !strings.Contains(string(data), "chamber-ci-stderr") {
				t.Fatalf("state=%s error=%v\n%s", state, err, data)
			}
			if err := cleanupJob(ctx, s.config, j); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(jobWorkspace(s.config, j.ID)); !os.IsNotExist(err) {
				t.Fatalf("workspace remains: %v", err)
			}
			t.Logf("Dockerfile built and ran with exit %d; stdout/stderr captured and workspace removed", exitCode)
		})
	}
}

func TestDroppedContainerCleanupIntegration(t *testing.T) {
	requireIntegration(t)
	s := testServer(t)
	j := admit(t, s, strings.Repeat("a", 40))
	if claimed, err := s.store.Claim(context.Background(), 1); err != nil || claimed == nil {
		t.Fatalf("claim: %v", err)
	}
	work := jobWorkspace(s.config, j.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	imageConfig := chamberImage.DefaultConfig(work)
	imageConfig.TmpRoot = filepath.Join(work, "i")
	store, err := chamberImageFactory.NewStore(imageConfig)
	if err != nil {
		t.Fatal(err)
	}
	image, err := store.Pull(ctx, chamberImage.PullRequest{Reference: "busybox:1.37"})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := chamberRuntimeFactory.NewRuntime(ctx, runtimeConfig(s.config, work, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	container, err := rt.RunImage(ctx, image, chamberRuntime.RunImageRequest{ContainerID: j.ID, Process: chamberBundle.ProcessSpec{Args: []string{"/bin/sh", "-c", "echo dropped-job; sleep 60"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = container.Delete(cleanupCtx, true)
		_, _ = container.Wait(cleanupCtx)
		_ = cleanupJob(cleanupCtx, s.config, j)
	}()
	eventually(t, func() bool {
		status, err := container.State(ctx)
		return err == nil && status.Status == chamberRuntime.ContainerStatusRunning
	})
	reported := make(chan string, 1)
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		reported <- body["state"]
		w.WriteHeader(201)
	}))
	defer github.Close()
	r := s.config.Repositories["test"]
	r.APIURL = github.URL
	s.config.Repositories["test"] = r
	// Reopen only the durable record and SDK handle, as a replacement server does.
	restarted, err := newServer(s.config, s.passcode, s.store)
	if err != nil {
		t.Fatal(err)
	}
	restarted.reportJobUpdates(ctx)
	_, _ = container.Wait(ctx)
	reportedState := ""
	select {
	case reportedState = <-reported:
	default:
	}
	if storedJob(t, restarted, j.ID).State != "error" || !storedJob(t, restarted, j.ID).Cleaned || !storedJob(t, restarted, j.ID).Reported || reportedState != "error" {
		t.Fatalf("dropped job not reconciled: %+v status=%q", storedJob(t, restarted, j.ID), reportedState)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("runtime/bundle workspace still exists: %v", err)
	}
	t.Log("recovered running job: error status delivered to mock GitHub; real runc container terminated and workspace removed")
}
