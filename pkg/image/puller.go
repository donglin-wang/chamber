package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrRootRequired = errors.New("image root is required")

type PullRequest struct {
	Reference   string
	Destination string
}

type PulledImage struct {
	Reference  string
	Digest     string
	LayoutPath string
	SizeBytes  int64
	PulledAt   time.Time
}

type Puller interface {
	// Pull writes a complete OCI image layout below Destination. It must write
	// to a temporary sibling first and rename only after verification succeeds.
	Pull(ctx context.Context, request PullRequest) (PulledImage, error)
}

func Destination(root string, reference string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", ErrRootRequired
	}
	sum := sha256.Sum256([]byte(reference))
	return filepath.Join(root, hex.EncodeToString(sum[:])), nil
}

func LayoutExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(path, "index.json"))
	return err == nil && !info.IsDir()
}
