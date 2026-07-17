package image_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImagePuller "github.com/donglin-wang/chamber/pkg/image/puller"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	"github.com/donglin-wang/chamber/pkg/shared/testutil"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

const busyboxReference = "index.docker.io/library/busybox:latest"

type pullerFactory func(t *testing.T) chamberImage.Puller

func TestPullerLocalContract(t *testing.T) {
	tests := map[string]pullerFactory{
		"puller": func(t *testing.T) chamberImage.Puller {
			t.Helper()
			return chamberImagePuller.New(localfs.NewDirectoryManager())
		},
	}

	for name, newPuller := range tests {
		t.Run(name, func(t *testing.T) {
			assertPullInvalidReference(t, newPuller)
			assertPullFetchFailureLeavesNoFinalLayout(t, newPuller)
			assertPullRenameFailureIsReturned(t, newPuller)
			assertPullSuccessReturnsDigestSizeAndUTCTime(t, newPuller, localImageReference(t))
			assertPullSuccessWithExplicitPlatformAndAuth(t, newPuller, localImageReference(t))
		})
	}
}

func TestImagePullerRealWorldBusybox(t *testing.T) {
	puller := chamberImagePuller.New(localfs.NewDirectoryManager())
	assertPullSuccessReturnsDigestSizeAndUTCTime(t, func(t *testing.T) chamberImage.Puller {
		t.Helper()
		return puller
	}, imageFixture{
		reference: busyboxReference,
	})
}

func assertPullInvalidReference(t *testing.T, newPuller pullerFactory) {
	t.Helper()

	puller := newPuller(t)

	_, err := puller.Pull(context.Background(), chamberImage.PullRequest{
		Reference:   "not a reference !!",
		Destination: filepath.Join(privateTempDir(t), "layout"),
	})
	if err == nil {
		t.Fatal("Pull() error = nil, want invalid reference error")
	}
}

func assertPullFetchFailureLeavesNoFinalLayout(t *testing.T, newPuller pullerFactory) {
	t.Helper()

	registry := testutil.NewFailingRegistry(t)
	destination := filepath.Join(privateTempDir(t), "layout")
	puller := newPuller(t)

	_, err := puller.Pull(context.Background(), chamberImage.PullRequest{
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

	_, err := puller.Pull(context.Background(), chamberImage.PullRequest{
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

	pulled, err := puller.Pull(context.Background(), chamberImage.PullRequest{
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
	assertLayoutHasImageRef(t, destination, image.reference)
}

func assertPullSuccessWithExplicitPlatformAndAuth(t *testing.T, newPuller pullerFactory, image imageFixture) {
	t.Helper()

	destination := filepath.Join(privateTempDir(t), "layout")
	puller := newPuller(t)

	pulled, err := puller.Pull(context.Background(), chamberImage.PullRequest{
		Reference:   image.reference,
		Destination: destination,
		Platform: chamberImage.Platform{
			OS:           "linux",
			Architecture: runtime.GOARCH,
		},
		Auth: &chamberImage.Auth{
			Username: "user",
			Password: "pass",
		},
	})
	if err != nil {
		t.Fatalf("Pull(explicit platform/auth) error = %v", err)
	}
	if pulled.LayoutPath != destination {
		t.Fatalf("LayoutPath = %q, want %q", pulled.LayoutPath, destination)
	}
	assertLayoutHasImageRef(t, destination, image.reference)
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

func assertLayoutHasImageRef(t *testing.T, path string, reference string) {
	t.Helper()

	layoutPath, err := layout.FromPath(path)
	if err != nil {
		t.Fatalf("layout.FromPath(%q) error = %v", path, err)
	}
	index, err := layoutPath.ImageIndex()
	if err != nil {
		t.Fatalf("ImageIndex() error = %v", err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		t.Fatalf("IndexManifest() error = %v", err)
	}
	for _, descriptor := range manifest.Manifests {
		if descriptor.Annotations[imagespec.AnnotationRefName] == reference {
			return
		}
	}
	t.Fatalf("OCI layout ref annotation %q not found for reference %q", imagespec.AnnotationRefName, reference)
}
