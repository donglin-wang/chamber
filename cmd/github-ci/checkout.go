package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/donglin-wang/chamber/pkg/shared/subprocess"
)

func checkoutExactSHA(ctx context.Context, checkoutDir string, remote string, sha string) error {
	if err := os.MkdirAll(checkoutDir, 0700); err != nil {
		return fmt.Errorf("create checkout directory: %w", err)
	}
	if _, err := runGit(ctx, checkoutDir, "init"); err != nil {
		return err
	}
	if _, err := runGit(ctx, checkoutDir, "remote", "add", "origin", remote); err != nil {
		return err
	}
	if _, err := runGit(ctx, checkoutDir, "fetch", "--depth=1", "origin", sha); err != nil {
		return err
	}
	if _, err := runGit(ctx, checkoutDir, "checkout", "--detach", "FETCH_HEAD"); err != nil {
		return err
	}
	output, err := runGit(ctx, checkoutDir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	actual := strings.TrimSpace(string(output))
	if actual != sha {
		return fmt.Errorf("checkout HEAD %q does not match requested SHA %q", actual, sha)
	}
	return nil
}

func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	command := subprocess.CommandContext(ctx, "git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("run git %s in %s: %w: %s", strings.Join(args, " "), filepath.Clean(dir), err, string(bytes.TrimSpace(output)))
	}
	return output, nil
}
