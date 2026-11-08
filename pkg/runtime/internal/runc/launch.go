package runc

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

// runc atomically publishes --pid-file after container.Run has completed OCI
// initialization. Unlike polling state, this acknowledgment survives fast exits.
// It confirms runtime initialization, not application readiness.
func (c *runcContainer) awaitLaunch(ctx context.Context, pidPath string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil || pid <= 0 {
				return fmt.Errorf("%w: invalid runtime launch acknowledgment %q", chamberErrors.ErrRuntimeStartFailed, data)
			}
			info, err := os.Stat(pidPath)
			if err != nil {
				return fmt.Errorf("%w: inspect launch acknowledgment: %w", chamberErrors.ErrRuntimeStartFailed, err)
			}
			startedAt := info.ModTime().UTC()
			c.startedAt = &startedAt
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("%w: read launch acknowledgment: %w", chamberErrors.ErrRuntimeStartFailed, err)
		}
		select {
		case <-c.done:
			// Check again after Wait: the PID file may have appeared between
			// the read above and runc exiting.
			if _, err := os.Stat(pidPath); err == nil {
				continue
			}
			stderr, _ := os.ReadFile(c.stderrPath)
			return fmt.Errorf("%w: runc exited before acknowledging container initialization: %s", chamberErrors.ErrRuntimeStartFailed, strings.TrimSpace(string(stderr)))
		case <-ctx.Done():
			return fmt.Errorf("%w: runtime launch canceled: %w", chamberErrors.ErrCanceled, ctx.Err())
		case <-ticker.C:
		}
	}
}
