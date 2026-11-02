package main

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func buildUpdate(ctx context.Context, c config) (candidate string, err error) {
	output, err := openLog(filepath.Join(c.Root, "update.log"), c.MaxLogBytes)
	if err != nil {
		return "", err
	}
	defer output.Close()
	defer func() {
		if err != nil {
			fmt.Fprintln(output, "Update failed:", err)
		}
	}()
	source, err := os.MkdirTemp(c.Root, "update-source-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(source)
	fmt.Fprintln(output, "Fetching update source", c.Update.Ref)
	if err := checkout(ctx, source, c.Update.CloneURL, c.Update.Ref, output); err != nil {
		return "", err
	}
	var head strings.Builder
	if err := runCommand(ctx, "", &head, "git", "-C", source, "rev-parse", "HEAD"); err != nil {
		return "", err
	}
	sha := strings.TrimSpace(head.String())
	if !commitSHA.MatchString(sha) {
		return "", fmt.Errorf("update source returned an invalid commit")
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	stage, err := os.CreateTemp(filepath.Dir(executable), ".chamber-ci-update-*")
	if err != nil {
		return "", fmt.Errorf("executable directory must be user-writable: %w", err)
	}
	candidate = stage.Name()
	if err := stage.Close(); err != nil {
		os.Remove(candidate)
		return "", err
	}
	defer func() {
		if err != nil {
			os.Remove(candidate)
		}
	}()
	fmt.Fprintln(output, "Building CI revision", sha)
	if err := runCommand(ctx, source, output, "go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-X main.revision="+sha, "-o", candidate, "./cmd/ci"); err != nil {
		return candidate, err
	}
	f, err := os.Open(candidate)
	if err != nil {
		return candidate, err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return candidate, err
	}
	fmt.Fprintln(output, "Update built; waiting for running jobs to finish")
	return candidate, nil
}

// Replace the executable atomically. If exec itself fails, restore the binary
// and return to the caller, which resumes serving with the same socket and lock.
func replaceProcess(candidate, executable, configPath string, socket, lock *os.File, execProcess func(string, []string, []string) error) error {
	backup := executable + ".previous"
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Link(executable, backup); err != nil {
		return err
	}
	if err := os.Rename(candidate, executable); err != nil {
		return err
	}
	restore := func(err error) error {
		return errors.Join(err, os.Rename(backup, executable), syncDirectory(filepath.Dir(executable)))
	}
	if err := syncDirectory(filepath.Dir(executable)); err != nil {
		return restore(err)
	}
	// All subprocesses have stopped. Only these two descriptors may survive exec.
	for _, file := range []*os.File{socket, lock} {
		if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFD, 0); err != nil {
			return restore(err)
		}
		defer syscall.CloseOnExec(int(file.Fd()))
	}
	args := []string{executable, "-config", configPath, "-listener-fd", strconv.Itoa(int(socket.Fd())), "-lock-fd", strconv.Itoa(int(lock.Fd()))}
	return restore(execProcess(executable, args, os.Environ()))
}
