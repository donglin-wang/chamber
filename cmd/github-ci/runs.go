package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

type runStatus string

const (
	runStatusRunning   runStatus = "running"
	runStatusSucceeded runStatus = "succeeded"
	runStatusFailed    runStatus = "failed"
	runStatusErrored   runStatus = "errored"
)

type runRecord struct {
	RunID       string            `json:"run_id"`
	SHA         string            `json:"sha"`
	Ref         string            `json:"ref"`
	Repository  string            `json:"repository"`
	Status      runStatus         `json:"status"`
	StartedAt   time.Time         `json:"started_at"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	ExitCode    int               `json:"exit_code"`
	Error       string            `json:"error,omitempty"`
	Logs        map[string]string `json:"logs,omitempty"`
}

var safePathComponentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func isSafePathComponent(value string) bool {
	return safePathComponentPattern.MatchString(value) && value != "." && value != ".."
}

func runLogPath(root string, runID string, job string, stream string) (string, error) {
	if !isSafePathComponent(runID) {
		return "", fmt.Errorf("invalid run ID %q", runID)
	}
	if !isSafePathComponent(job) {
		return "", fmt.Errorf("invalid job name %q", job)
	}
	if stream != "stdout" && stream != "stderr" {
		return "", fmt.Errorf("invalid log stream %q", stream)
	}
	return filepath.Join(root, "runs", runID, "logs", job+"."+stream), nil
}

func writeRunRecord(root string, record runRecord) error {
	if !isSafePathComponent(record.RunID) {
		return fmt.Errorf("invalid run ID %q", record.RunID)
	}
	path := filepath.Join(root, "runs", record.RunID, "status.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create run directory: %w", err)
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode run record: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".status-*.json")
	if err != nil {
		return fmt.Errorf("create temporary run record: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write run record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close run record: %w", err)
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod run record: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("publish run record: %w", err)
	}
	return nil
}

func readRunRecord(root string, runID string) (runRecord, error) {
	if !isSafePathComponent(runID) {
		return runRecord{}, fmt.Errorf("invalid run ID %q", runID)
	}
	path := filepath.Join(root, "runs", runID, "status.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return runRecord{}, err
	}
	var record runRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return runRecord{}, fmt.Errorf("decode run record: %w", err)
	}
	return record, nil
}
