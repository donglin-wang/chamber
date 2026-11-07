package testutil

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// LiveProcessesMatching finds Linux processes whose command line contains token.
// Callers use a unique temporary-directory name to avoid matching other jobs.
func LiveProcessesMatching(token string) []int {
	entries, _ := os.ReadDir("/proc")
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if strings.Contains(string(cmdline), token) && ProcessAlive(pid) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// ProcessAlive excludes zombies, which no longer execute or own open files.
func ProcessAlive(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	_, fields, ok := strings.Cut(string(data), ") ")
	return ok && len(fields) > 0 && fields[0] != 'Z' && fields[0] != 'X'
}

// Eventually waits for an observable integration-test condition with a deadline.
func Eventually(t testing.TB, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for integration-test condition")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
