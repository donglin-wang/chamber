package image_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chimage "github.com/donglin-wang/chamber/internal/image"
	"github.com/donglin-wang/chamber/internal/image/gocontainerregistry"
	"github.com/donglin-wang/chamber/internal/testutil"
)

const busyboxReference = "index.docker.io/library/busybox:latest"

type pullerFactory func(t *testing.T) chimage.Puller

func TestPullerLocalContract(t *testing.T) {
	tests := map[string]pullerFactory{
		"gocontainerregistry": func(t *testing.T) chimage.Puller {
			t.Helper()
			return gocontainerregistry.New()
		},
	}

	for name, newPuller := range tests {
		t.Run(name, func(t *testing.T) {
			assertPullInvalidReference(t, newPuller)
			assertPullUnsupportedPlatform(t, newPuller)
			assertPullFetchFailureLeavesNoFinalLayout(t, newPuller)
			assertPullRenameFailureIsReturned(t, newPuller)
			assertPullSuccessReturnsDigestSizeAndUTCTime(t, newPuller, localImageReference(t))
		})
	}
}

func TestGoContainerRegistryPullerRealWorldBusybox(t *testing.T) {
	puller := gocontainerregistry.New()
	assertPullSuccessReturnsDigestSizeAndUTCTime(t, func(t *testing.T) chimage.Puller {
		t.Helper()
		return puller
	}, imageFixture{
		reference: busyboxReference,
	})
}

func assertPullInvalidReference(t *testing.T, newPuller pullerFactory) {
	t.Helper()

	puller := newPuller(t)

	_, err := puller.Pull(context.Background(), chimage.PullRequest{
		Reference:   "not a reference !!",
		Destination: filepath.Join(privateTempDir(t), "layout"),
	})
	if err == nil {
		t.Fatal("Pull() error = nil, want invalid reference error")
	}
}

func assertPullUnsupportedPlatform(t *testing.T, newPuller pullerFactory) {
	t.Helper()

	puller := newPuller(t)

	_, err := puller.Pull(context.Background(), chimage.PullRequest{
		Reference:   "docker.io/library/alpine:latest",
		Destination: filepath.Join(privateTempDir(t), "layout"),
		Platform:    "windows/amd64",
	})
	if err == nil {
		t.Fatal("Pull() error = nil, want unsupported platform error")
	}
}

func assertPullFetchFailureLeavesNoFinalLayout(t *testing.T, newPuller pullerFactory) {
	t.Helper()

	registry := testutil.NewFailingRegistry(t)
	destination := filepath.Join(privateTempDir(t), "layout")
	puller := newPuller(t)

	_, err := puller.Pull(context.Background(), chimage.PullRequest{
		Reference:   registry.Reference(t, "library/busybox", "latest"),
		Destination: destination,
	})
	if err == nil {
		t.Fatal("Pull() error = nil, want registry failure")
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("final layout stat error = %v, want %v", statErr, os.ErrNotExist)
	}
}

func assertPullRenameFailureIsReturned(t *testing.T, newPuller pullerFactory) {
	t.Helper()

	root := privateTempDir(t)
	destination := filepath.Join(root, "layout")
	if err := os.MkdirAll(destination, 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(destination, "existing"), []byte("already here"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	image := localImageReference(t)
	puller := newPuller(t)

	_, err := puller.Pull(context.Background(), chimage.PullRequest{
		Reference:   image.reference,
		Destination: destination,
	})
	if err == nil {
		t.Fatal("Pull() error = nil, want rename error")
	}
	if _, statErr := os.Stat(filepath.Join(destination, "existing")); statErr != nil {
		t.Fatalf("existing final path changed after rename failure: %v", statErr)
	}
}

func assertPullSuccessReturnsDigestSizeAndUTCTime(t *testing.T, newPuller pullerFactory, image imageFixture) {
	t.Helper()

	destination := filepath.Join(privateTempDir(t), "layout")
	puller := newPuller(t)
	before := time.Now().UTC()

	pulled, err := puller.Pull(context.Background(), chimage.PullRequest{
		Reference:   image.reference,
		Destination: destination,
	})
	if err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	after := time.Now().UTC()

	if pulled.Reference != image.reference {
		t.Fatalf("Reference = %q, want %q", pulled.Reference, image.reference)
	}
	if image.digest != "" && pulled.Digest != image.digest {
		t.Fatalf("Digest = %q, want %q", pulled.Digest, image.digest)
	}
	if !strings.HasPrefix(pulled.Digest, "sha256:") {
		t.Fatalf("Digest = %q, want sha256 digest", pulled.Digest)
	}
	if pulled.LayoutPath != destination {
		t.Fatalf("LayoutPath = %q, want %q", pulled.LayoutPath, destination)
	}
	if pulled.SizeBytes <= 0 {
		t.Fatalf("SizeBytes = %d, want positive size", pulled.SizeBytes)
	}
	if pulled.PulledAt.Location() != time.UTC {
		t.Fatalf("PulledAt location = %v, want UTC", pulled.PulledAt.Location())
	}
	if pulled.PulledAt.Before(before) || pulled.PulledAt.After(after) {
		t.Fatalf("PulledAt = %v, want between %v and %v", pulled.PulledAt, before, after)
	}
	if _, err := os.Stat(filepath.Join(destination, "index.json")); err != nil {
		t.Fatalf("final OCI layout missing index.json: %v", err)
	}
}

type imageFixture struct {
	reference string
	digest    string
}

func localImageReference(t *testing.T) imageFixture {
	t.Helper()

	registry := testutil.NewFakeRegistry(t)
	reference, digest := registry.PushRandomImage(t, "library/busybox", "latest")
	return imageFixture{
		reference: reference,
		digest:    digest.String(),
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()

	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatalf("Chmod(%q) error = %v", path, err)
	}
	return path
}
