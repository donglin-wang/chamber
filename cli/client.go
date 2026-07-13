package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	defaultHTTPBaseURL = "http://chamber"
)

type getenvFunc func(string) string

type commandKind string

const (
	commandPull           commandKind = "pull"
	commandRun            commandKind = "run"
	commandListContainers commandKind = "containers-ls"
	commandLogs           commandKind = "logs"
)

type command struct {
	kind        commandKind
	socketPath  string
	addr        string
	reference   string
	image       string
	args        []string
	containerID string
	logStream   string
}

type Client struct {
	baseURL    string
	httpClient *http.Client
}

type PullRequest struct {
	Reference string `json:"reference"`
}

type PullResponse struct {
	OperationID string    `json:"operation_id"`
	Reference   string    `json:"reference"`
	Digest      string    `json:"digest"`
	PulledAt    time.Time `json:"pulled_at"`
}

type RunRequest struct {
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
}

type RunResponse struct {
	OperationID string `json:"operation_id"`
	ID          string `json:"id"`
	ImageDigest string `json:"image_digest"`
	State       string `json:"state"`
}

type ListContainersResponse struct {
	Containers []ContainerResponse `json:"containers"`
}

type ContainerResponse struct {
	ID          string    `json:"id"`
	OperationID string    `json:"operation_id"`
	Image       string    `json:"image"`
	ImageDigest string    `json:"image_digest"`
	Runtime     string    `json:"runtime"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	ErrorCode   string    `json:"error_code,omitempty"`
}

type ErrorResponse struct {
	OperationID string `json:"operation_id,omitempty"`
	Code        string `json:"code"`
	Message     string `json:"message"`
}

func Run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, getenv getenvFunc) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if getenv == nil {
		getenv = os.Getenv
	}

	parsed, err := parseArgs(args)
	if err != nil {
		return err
	}
	client, err := newClient(parsed.socketPath, parsed.addr, getenv)
	if err != nil {
		return err
	}

	switch parsed.kind {
	case commandPull:
		response, err := client.Pull(ctx, PullRequest{Reference: parsed.reference})
		if err != nil {
			return err
		}
		printPullResponse(stdout, response)
	case commandRun:
		response, err := client.Run(ctx, RunRequest{Image: parsed.image, Command: parsed.args})
		if err != nil {
			return err
		}
		printRunResponse(stdout, response, defaultLogPaths(parsed, response.ID, getenv))
	case commandListContainers:
		response, err := client.ListContainers(ctx)
		if err != nil {
			return err
		}
		printContainers(stdout, response.Containers)
	case commandLogs:
		content, err := client.ContainerLogs(ctx, parsed.containerID, parsed.logStream)
		if err != nil {
			return err
		}
		_, _ = stdout.Write(content)
	default:
		return fmt.Errorf("unknown command %q", parsed.kind)
	}
	return nil
}

func parseArgs(args []string) (command, error) {
	var parsed command
	fs := flag.NewFlagSet("chamber", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&parsed.socketPath, "socket", "", "Unix socket path")
	fs.StringVar(&parsed.addr, "addr", "", "HTTP daemon address, for example http://127.0.0.1:8080")
	if err := fs.Parse(args); err != nil {
		return command{}, err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		return command{}, fmt.Errorf("command is required")
	}
	switch rest[0] {
	case "pull":
		if len(rest) != 2 {
			return command{}, fmt.Errorf("usage: chamber pull IMAGE")
		}
		parsed.kind = commandPull
		parsed.reference = strings.TrimSpace(rest[1])
		if parsed.reference == "" {
			return command{}, fmt.Errorf("image reference is required")
		}
	case "run":
		if len(rest) < 4 || rest[2] != "--" {
			return command{}, fmt.Errorf("usage: chamber run IMAGE -- COMMAND [ARG...]")
		}
		parsed.kind = commandRun
		parsed.image = strings.TrimSpace(rest[1])
		parsed.args = rest[3:]
		if parsed.image == "" {
			return command{}, fmt.Errorf("image reference is required")
		}
		if len(parsed.args) == 0 || strings.TrimSpace(parsed.args[0]) == "" {
			return command{}, fmt.Errorf("command is required")
		}
	case "containers":
		if len(rest) != 2 || rest[1] != "ls" {
			return command{}, fmt.Errorf("usage: chamber containers ls")
		}
		parsed.kind = commandListContainers
	case "logs":
		containerID, stream, err := parseLogsArgs(rest[1:])
		if err != nil {
			return command{}, err
		}
		parsed.kind = commandLogs
		parsed.containerID = containerID
		parsed.logStream = stream
	default:
		return command{}, fmt.Errorf("unknown command %q", rest[0])
	}
	return parsed, nil
}

func parseLogsArgs(args []string) (containerID string, stream string, err error) {
	stream = "stdout"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--stderr":
			stream = "stderr"
		case "--stdout":
			stream = "stdout"
		case "--stream":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("usage: chamber logs CONTAINER [--stderr|--stream stdout|stderr]")
			}
			i++
			stream = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", "", fmt.Errorf("unknown logs flag %q", args[i])
			}
			if containerID != "" {
				return "", "", fmt.Errorf("usage: chamber logs CONTAINER [--stderr|--stream stdout|stderr]")
			}
			containerID = strings.TrimSpace(args[i])
		}
	}
	if containerID == "" {
		return "", "", fmt.Errorf("usage: chamber logs CONTAINER [--stderr|--stream stdout|stderr]")
	}
	if stream != "stdout" && stream != "stderr" {
		return "", "", fmt.Errorf("--stream must be stdout or stderr")
	}
	return containerID, stream, nil
}

func newClient(socketPath string, addr string, getenv getenvFunc) (*Client, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if socketPath != "" && addr != "" {
		return nil, fmt.Errorf("--socket and --addr cannot both be set")
	}
	if addr != "" {
		baseURL, err := normalizeHTTPAddr(addr)
		if err != nil {
			return nil, err
		}
		return &Client{
			baseURL:    baseURL,
			httpClient: &http.Client{},
		}, nil
	}
	if socketPath == "" {
		var err error
		socketPath, err = defaultSocketPath(getenv)
		if err != nil {
			return nil, err
		}
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		baseURL: defaultHTTPBaseURL,
		httpClient: &http.Client{
			Transport: transport,
		},
	}, nil
}

func normalizeHTTPAddr(addr string) (string, error) {
	parsed, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("parse --addr: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("--addr must start with http:// or https://")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("--addr must include a host")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func defaultSocketPath(getenv getenvFunc) (string, error) {
	root, err := defaultRootPath(getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "run", "chamber.sock"), nil
}

func defaultContainerRoot(getenv getenvFunc) (string, error) {
	root, err := defaultRootPath(getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "containers"), nil
}

func defaultRootPath(getenv getenvFunc) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	root := ""
	if xdg := getenv("XDG_DATA_HOME"); xdg != "" {
		root = filepath.Join(xdg, "chamber")
	} else if home := getenv("HOME"); home != "" {
		root = filepath.Join(home, ".local", "share", "chamber")
	} else {
		return "", fmt.Errorf("cannot derive default path: neither XDG_DATA_HOME nor HOME is set")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("make default path absolute: %w", err)
	}
	return abs, nil
}

func (c *Client) Pull(ctx context.Context, request PullRequest) (PullResponse, error) {
	var response PullResponse
	if err := c.postJSON(ctx, "/v1/images/pull", request, http.StatusOK, &response); err != nil {
		return PullResponse{}, err
	}
	return response, nil
}

func (c *Client) Run(ctx context.Context, request RunRequest) (RunResponse, error) {
	var response RunResponse
	if err := c.postJSON(ctx, "/v1/containers/run", request, http.StatusCreated, &response); err != nil {
		return RunResponse{}, err
	}
	return response, nil
}

func (c *Client) ListContainers(ctx context.Context) (ListContainersResponse, error) {
	var response ListContainersResponse
	if err := c.getJSON(ctx, "/v1/containers", http.StatusOK, &response); err != nil {
		return ListContainersResponse{}, err
	}
	return response, nil
}

func (c *Client) ContainerLogs(ctx context.Context, containerID string, stream string) ([]byte, error) {
	if stream == "" {
		stream = "stdout"
	}
	path := "/v1/containers/" + url.PathEscape(containerID) + "/logs?stream=" + url.QueryEscape(stream)
	return c.getBytes(ctx, path, http.StatusOK)
}

func (c *Client) postJSON(ctx context.Context, path string, request any, successStatus int, response any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")

	httpResponse, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("call chamberd: %w", err)
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode != successStatus {
		return decodeAPIError(httpResponse)
	}
	if err := json.NewDecoder(httpResponse.Body).Decode(response); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) getJSON(ctx context.Context, path string, successStatus int, response any) error {
	content, err := c.getBytes(ctx, path, successStatus)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(content, response); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) getBytes(ctx context.Context, path string, successStatus int) ([]byte, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpRequest.Header.Set("Accept", "application/json, text/plain")

	httpResponse, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("call chamberd: %w", err)
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode != successStatus {
		return nil, decodeAPIError(httpResponse)
	}
	content, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return content, nil
}

func decodeAPIError(response *http.Response) error {
	var daemonErr ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&daemonErr); err != nil {
		return fmt.Errorf("chamberd returned HTTP %d", response.StatusCode)
	}
	if daemonErr.Code == "" && daemonErr.Message == "" {
		return fmt.Errorf("chamberd returned HTTP %d", response.StatusCode)
	}
	if daemonErr.OperationID != "" {
		return fmt.Errorf("chamberd returned %s for operation %s: %s", daemonErr.Code, daemonErr.OperationID, daemonErr.Message)
	}
	return fmt.Errorf("chamberd returned %s: %s", daemonErr.Code, daemonErr.Message)
}

func printPullResponse(stdout io.Writer, response PullResponse) {
	fmt.Fprintf(stdout, "reference: %s\n", response.Reference)
	fmt.Fprintf(stdout, "digest: %s\n", response.Digest)
	fmt.Fprintf(stdout, "operation: %s\n", response.OperationID)
}

type logPaths struct {
	stdout string
	stderr string
}

func defaultLogPaths(parsed command, containerID string, getenv getenvFunc) logPaths {
	if parsed.addr != "" || parsed.socketPath != "" || containerID == "" {
		return logPaths{}
	}
	containerRoot, err := defaultContainerRoot(getenv)
	if err != nil {
		return logPaths{}
	}
	return logPaths{
		stdout: filepath.Join(containerRoot, containerID, "stdout.log"),
		stderr: filepath.Join(containerRoot, containerID, "stderr.log"),
	}
}

func printRunResponse(stdout io.Writer, response RunResponse, logs logPaths) {
	fmt.Fprintf(stdout, "operation: %s\n", response.OperationID)
	fmt.Fprintf(stdout, "container: %s\n", response.ID)
	fmt.Fprintf(stdout, "state: %s\n", response.State)
	if logs.stdout != "" {
		fmt.Fprintf(stdout, "stdout_log: %s\n", logs.stdout)
		fmt.Fprintf(stdout, "stderr_log: %s\n", logs.stderr)
	}
}

func printContainers(stdout io.Writer, containers []ContainerResponse) {
	table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tIMAGE\tSTATE\tEXIT\tOPERATION")
	for _, container := range containers {
		exit := "-"
		if container.ExitCode != nil {
			exit = fmt.Sprintf("%d", *container.ExitCode)
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", container.ID, container.Image, container.State, exit, container.OperationID)
	}
	_ = table.Flush()
}
