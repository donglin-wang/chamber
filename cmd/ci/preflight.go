package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	"github.com/donglin-wang/chamber/pkg/shared/subprocess"
)

// This path reads state but never recovers jobs or generates
// a passcode. It can safely run while the current server is still working.
func checkStartup(ctx context.Context, c config) error {
	for _, path := range []string{c.Root, c.WorkRoot, filepath.Join(c.Root, "logs")} {
		if err := hostfs.CheckPrivateDirectory(path); err != nil {
			return err
		}
	}
	if _, err := readPasscode(c.Root); err != nil {
		return err
	}
	marker, err := os.ReadFile(filepath.Join(c.Root, "initialized"))
	if err != nil {
		return err
	}
	if string(marker) != "1\n" {
		return fmt.Errorf("invalid CI initialization marker")
	}
	uri := url.URL{Scheme: "file", Path: filepath.Join(c.Root, "jobs.sqlite")}
	uri.RawQuery = url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)"}}.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return err
	}
	defer db.Close()
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("unsupported CI database schema version %d", version)
	}
	rows, err := db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs LIMIT 0`)
	if err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return validateStoredJobs(ctx, c, &sqliteStore{db: db})
}

// A candidate exposes health only after parsing config and checking its state.
// It does not claim jobs, recover records, or replace the update password.
func checkStartupHealth(ctx context.Context, c config) error {
	if err := checkStartup(ctx, c); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, revision) })
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = server.Close() }()
	fmt.Fprintln(os.Stdout, listener.Addr())
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func checkCandidate(ctx context.Context, path, configPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := subprocess.CommandContext(ctx, path, "-config", configPath, "-check")
	subprocess.SetProcessGroup(command)
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = 5 * time.Second
	command.Stderr = os.Stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := command.Start(); err != nil {
		return "", err
	}
	defer func() { _ = command.Cancel(); _ = command.Wait() }()
	address, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("candidate did not start its health endpoint: %w", err)
	}
	// Only contact the candidate's private loopback probe.
	address = strings.TrimSpace(address)
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		return "", fmt.Errorf("invalid candidate health address %q", address)
	}
	request, err := http.NewRequestWithContext(ctx, "GET", "http://"+address+"/health", nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("candidate health: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 41))
	if err != nil || response.StatusCode != http.StatusOK || !commitSHA.Match(body) {
		return "", fmt.Errorf("candidate failed its health check")
	}
	return string(body), nil
}
