package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseArgsBuildsPullAndRunCommands(t *testing.T) {
	pull, err := parseArgs([]string{"--socket", "/tmp/chamber.sock", "pull", "docker.io/library/alpine:latest"})
	if err != nil {
		t.Fatalf("parse pull args: %v", err)
	}
	if pull.kind != commandPull || pull.socketPath != "/tmp/chamber.sock" || pull.reference != "docker.io/library/alpine:latest" {
		t.Fatalf("pull command = %#v, want socket pull command", pull)
	}

	run, err := parseArgs([]string{
		"--addr", "http://127.0.0.1:8080",
		"run", "docker.io/library/alpine:latest",
		"--", "/bin/sh", "-c", "id && echo chamber",
	})
	if err != nil {
		t.Fatalf("parse run args: %v", err)
	}
	if run.kind != commandRun || run.addr != "http://127.0.0.1:8080" || run.image != "docker.io/library/alpine:latest" {
		t.Fatalf("run command = %#v, want TCP run command", run)
	}
	if want := []string{"/bin/sh", "-c", "id && echo chamber"}; !reflect.DeepEqual(run.args, want) {
		t.Fatalf("run args = %#v, want %#v", run.args, want)
	}

	list, err := parseArgs([]string{"containers", "ls"})
	if err != nil {
		t.Fatalf("parse containers ls args: %v", err)
	}
	if list.kind != commandListContainers {
		t.Fatalf("list command kind = %q, want %q", list.kind, commandListContainers)
	}

	logs, err := parseArgs([]string{"logs", "ctr-run", "--stderr"})
	if err != nil {
		t.Fatalf("parse logs args: %v", err)
	}
	if logs.kind != commandLogs || logs.containerID != "ctr-run" || logs.logStream != "stderr" {
		t.Fatalf("logs command = %#v, want stderr logs command", logs)
	}
}

func TestParseArgsRejectsInvalidCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing command", args: nil, want: "command is required"},
		{name: "unknown command", args: []string{"ps"}, want: `unknown command "ps"`},
		{name: "pull missing image", args: []string{"pull"}, want: "usage: chamber pull IMAGE"},
		{name: "run missing separator", args: []string{"run", "alpine", "/bin/sh"}, want: "usage: chamber run IMAGE -- COMMAND"},
		{name: "run missing command", args: []string{"run", "alpine", "--"}, want: "usage: chamber run IMAGE -- COMMAND"},
		{name: "containers invalid subcommand", args: []string{"containers", "inspect"}, want: "usage: chamber containers ls"},
		{name: "logs missing id", args: []string{"logs"}, want: "usage: chamber logs CONTAINER"},
		{name: "logs invalid stream", args: []string{"logs", "ctr", "--stream", "weird"}, want: "--stream must be stdout or stderr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseArgs(tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("parseArgs error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestNewClientBuildsUnixAndTCPTransports(t *testing.T) {
	unixClient, err := newClient("/tmp/chamber.sock", "", mapGetenv(nil))
	if err != nil {
		t.Fatalf("new unix client: %v", err)
	}
	if unixClient.baseURL != defaultHTTPBaseURL {
		t.Fatalf("unix baseURL = %q, want %q", unixClient.baseURL, defaultHTTPBaseURL)
	}
	if unixClient.httpClient.Transport == nil {
		t.Fatalf("unix client transport is nil")
	}

	tcpClient, err := newClient("", "http://127.0.0.1:8080/", mapGetenv(nil))
	if err != nil {
		t.Fatalf("new TCP client: %v", err)
	}
	if tcpClient.baseURL != "http://127.0.0.1:8080" {
		t.Fatalf("TCP baseURL = %q, want trimmed address", tcpClient.baseURL)
	}
	if tcpClient.httpClient.Transport != nil {
		t.Fatalf("TCP client transport = %#v, want default transport", tcpClient.httpClient.Transport)
	}

	if _, err := newClient("/tmp/chamber.sock", "http://127.0.0.1:8080", mapGetenv(nil)); err == nil {
		t.Fatalf("newClient accepted both --socket and --addr")
	}
}

func TestDefaultSocketPathUsesXDGThenHome(t *testing.T) {
	xdgSocket, err := defaultSocketPath(mapGetenv(map[string]string{
		"XDG_DATA_HOME": "/xdg",
		"HOME":          "/home/donglin",
	}))
	if err != nil {
		t.Fatalf("defaultSocketPath with XDG: %v", err)
	}
	if xdgSocket != filepath.Join("/xdg", "chamber", "run", "chamber.sock") {
		t.Fatalf("XDG socket = %q, want XDG chamber socket", xdgSocket)
	}

	homeSocket, err := defaultSocketPath(mapGetenv(map[string]string{
		"HOME": "/home/donglin",
	}))
	if err != nil {
		t.Fatalf("defaultSocketPath with HOME: %v", err)
	}
	if homeSocket != filepath.Join("/home/donglin", ".local", "share", "chamber", "run", "chamber.sock") {
		t.Fatalf("HOME socket = %q, want HOME chamber socket", homeSocket)
	}
}

func TestClientTCPModeRequestBodies(t *testing.T) {
	var pullBody PullRequest
	var runBody RunRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		switch r.URL.Path {
		case "/v1/images/pull":
			if r.Method != http.MethodPost {
				t.Fatalf("pull method = %s, want POST", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&pullBody); err != nil {
				t.Fatalf("decode pull body: %v", err)
			}
			writeJSON(t, w, http.StatusOK, PullResponse{
				OperationID: "op-pull",
				Reference:   pullBody.Reference,
				Digest:      "sha256:abc123",
				PulledAt:    time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC),
			})
		case "/v1/containers/run":
			if r.Method != http.MethodPost {
				t.Fatalf("run method = %s, want POST", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&runBody); err != nil {
				t.Fatalf("decode run body: %v", err)
			}
			writeJSON(t, w, http.StatusCreated, RunResponse{
				OperationID: "op-run",
				ID:          "ctr-run",
				ImageDigest: "sha256:def456",
				State:       "running",
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := newClient("", server.URL, mapGetenv(nil))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	pull, err := client.Pull(context.Background(), PullRequest{Reference: "docker.io/library/alpine:latest"})
	if err != nil {
		t.Fatalf("Pull returned error: %v", err)
	}
	if pull.OperationID != "op-pull" || pull.Reference != "docker.io/library/alpine:latest" {
		t.Fatalf("pull response = %#v, want daemon fields", pull)
	}
	run, err := client.Run(context.Background(), RunRequest{
		Image:   "docker.io/library/alpine:latest",
		Command: []string{"/bin/sh", "-c", "id && echo chamber"},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if run.OperationID != "op-run" || run.ID != "ctr-run" || run.State != "running" {
		t.Fatalf("run response = %#v, want daemon fields", run)
	}
	if pullBody.Reference != "docker.io/library/alpine:latest" {
		t.Fatalf("pull body = %#v, want reference", pullBody)
	}
	if runBody.Image != "docker.io/library/alpine:latest" || !reflect.DeepEqual(runBody.Command, []string{"/bin/sh", "-c", "id && echo chamber"}) {
		t.Fatalf("run body = %#v, want image and command", runBody)
	}
}

func TestClientListContainersAndLogsRequests(t *testing.T) {
	exitCode := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/containers":
			if r.Method != http.MethodGet {
				t.Fatalf("list method = %s, want GET", r.Method)
			}
			writeJSON(t, w, http.StatusOK, ListContainersResponse{
				Containers: []ContainerResponse{
					{
						ID:          "ctr-run",
						OperationID: "op-run",
						Image:       "docker.io/library/alpine:latest",
						ImageDigest: "sha256:def456",
						Runtime:     "runc",
						State:       "exited",
						ExitCode:    &exitCode,
					},
				},
			})
		case "/v1/containers/ctr-run/logs":
			if r.Method != http.MethodGet {
				t.Fatalf("logs method = %s, want GET", r.Method)
			}
			if got := r.URL.Query().Get("stream"); got != "stderr" {
				t.Fatalf("stream query = %q, want stderr", got)
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("hello stderr\n"))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := newClient("", server.URL, mapGetenv(nil))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	list, err := client.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("ListContainers returned error: %v", err)
	}
	if len(list.Containers) != 1 || list.Containers[0].ID != "ctr-run" || list.Containers[0].ExitCode == nil || *list.Containers[0].ExitCode != 0 {
		t.Fatalf("list response = %#v, want container", list)
	}
	logs, err := client.ContainerLogs(context.Background(), "ctr-run", "stderr")
	if err != nil {
		t.Fatalf("ContainerLogs returned error: %v", err)
	}
	if string(logs) != "hello stderr\n" {
		t.Fatalf("logs = %q, want stderr content", string(logs))
	}
}

func TestClientUnixSocketTransportCallsDaemon(t *testing.T) {
	dir := shortTempDir(t)
	socketPath := filepath.Join(dir, "chamber.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/pull" {
			t.Fatalf("path = %q, want /v1/images/pull", r.URL.Path)
		}
		writeJSON(t, w, http.StatusOK, PullResponse{
			OperationID: "op-unix",
			Reference:   "docker.io/library/alpine:latest",
			Digest:      "sha256:unix",
			PulledAt:    time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC),
		})
	})}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	defer func() {
		_ = server.Shutdown(context.Background())
		err := <-serveErr
		if err != nil && err != http.ErrServerClosed {
			t.Fatalf("server.Serve error = %v", err)
		}
	}()

	client, err := newClient(socketPath, "", mapGetenv(nil))
	if err != nil {
		t.Fatalf("new unix client: %v", err)
	}
	response, err := client.Pull(context.Background(), PullRequest{Reference: "docker.io/library/alpine:latest"})
	if err != nil {
		t.Fatalf("Pull returned error: %v", err)
	}
	if response.OperationID != "op-unix" || response.Digest != "sha256:unix" {
		t.Fatalf("response = %#v, want Unix socket daemon response", response)
	}
}

func TestRunUsesTCPModeAndPrintsResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/containers/run" {
			t.Fatalf("path = %q, want /v1/containers/run", r.URL.Path)
		}
		writeJSON(t, w, http.StatusCreated, RunResponse{
			OperationID: "op-run",
			ID:          "ctr-run",
			ImageDigest: "sha256:def456",
			State:       "running",
		})
	}))
	defer server.Close()

	var stdout bytes.Buffer
	err := Run(context.Background(), []string{
		"--addr", server.URL,
		"run", "docker.io/library/alpine:latest",
		"--", "/bin/sh", "-c", "id && echo chamber",
	}, &stdout, ioDiscard{}, mapGetenv(map[string]string{"HOME": "/home/donglin"}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	want := "operation: op-run\ncontainer: ctr-run\nstate: running\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunListsContainersAndPrintsLogs(t *testing.T) {
	exitCode := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/containers":
			writeJSON(t, w, http.StatusOK, ListContainersResponse{
				Containers: []ContainerResponse{
					{
						ID:          "ctr-run",
						OperationID: "op-run",
						Image:       "docker.io/library/alpine:latest",
						State:       "exited",
						ExitCode:    &exitCode,
					},
				},
			})
		case "/v1/containers/ctr-run/logs":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("uid=0(root)\nchamber\n"))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	var listOut bytes.Buffer
	err := Run(context.Background(), []string{
		"--addr", server.URL,
		"containers", "ls",
	}, &listOut, ioDiscard{}, mapGetenv(nil))
	if err != nil {
		t.Fatalf("Run(containers ls) returned error: %v", err)
	}
	for _, want := range []string{"ID", "IMAGE", "STATE", "EXIT", "ctr-run", "docker.io/library/alpine:latest", "exited", "0"} {
		if !strings.Contains(listOut.String(), want) {
			t.Fatalf("containers ls output = %q, want containing %q", listOut.String(), want)
		}
	}

	var logsOut bytes.Buffer
	err = Run(context.Background(), []string{
		"--addr", server.URL,
		"logs", "ctr-run",
	}, &logsOut, ioDiscard{}, mapGetenv(nil))
	if err != nil {
		t.Fatalf("Run(logs) returned error: %v", err)
	}
	if logsOut.String() != "uid=0(root)\nchamber\n" {
		t.Fatalf("logs output = %q, want raw logs", logsOut.String())
	}
}

func TestResponseFormatting(t *testing.T) {
	var pull bytes.Buffer
	printPullResponse(&pull, PullResponse{
		OperationID: "op-pull",
		Reference:   "docker.io/library/alpine:latest",
		Digest:      "sha256:abc123",
	})
	if want := "reference: docker.io/library/alpine:latest\ndigest: sha256:abc123\noperation: op-pull\n"; pull.String() != want {
		t.Fatalf("pull output = %q, want %q", pull.String(), want)
	}

	var run bytes.Buffer
	printRunResponse(&run, RunResponse{
		OperationID: "op-run",
		ID:          "ctr-run",
		State:       "exited",
	}, logPaths{
		stdout: "/home/donglin/.local/share/chamber/containers/ctr-run/stdout.log",
		stderr: "/home/donglin/.local/share/chamber/containers/ctr-run/stderr.log",
	})
	want := strings.Join([]string{
		"operation: op-run",
		"container: ctr-run",
		"state: exited",
		"stdout_log: /home/donglin/.local/share/chamber/containers/ctr-run/stdout.log",
		"stderr_log: /home/donglin/.local/share/chamber/containers/ctr-run/stderr.log",
		"",
	}, "\n")
	if run.String() != want {
		t.Fatalf("run output = %q, want %q", run.String(), want)
	}

	var list bytes.Buffer
	printContainers(&list, []ContainerResponse{
		{
			ID:          "ctr-run",
			OperationID: "op-run",
			Image:       "docker.io/library/alpine:latest",
			State:       "exited",
			ExitCode:    intPtr(0),
		},
	})
	for _, want := range []string{"ID", "IMAGE", "STATE", "EXIT", "OPERATION", "ctr-run", "docker.io/library/alpine:latest", "exited", "0", "op-run"} {
		if !strings.Contains(list.String(), want) {
			t.Fatalf("list output = %q, want containing %q", list.String(), want)
		}
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, response any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func mapGetenv(values map[string]string) getenvFunc {
	return func(key string) string {
		return values[key]
	}
}

func intPtr(value int) *int {
	return &value
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "chc-")
	if err != nil {
		t.Fatalf("create short temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	return dir
}
