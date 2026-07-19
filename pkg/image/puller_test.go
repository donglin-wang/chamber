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

type pullerFactory func(t *testing.T) (chamberImage.Puller, string)

func TestPullerLocalContract(t *testing.T) {
	tests := map[string]pullerFactory{
		"puller": func(t *testing.T) (chamberImage.Puller, string) {
			t.Helper()
			root := filepath.Join(privateTempDir(t), "images")
			puller, err := chamberImagePuller.New(chamberImage.Config{Root: root}, localfs.NewDirectoryManager())
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			return puller, root
		},
	}

	for name, newPuller := range tests {
		t.Run(name, func(t *testing.T) {
			assertPullInvalidReference(t, newPuller)
			assertPullFetchFailureLeavesNoFinalLayout(t, newPuller)
			assertPullInvalidExistingLayoutIsReturned(t, newPuller)
			assertPullSuccessReturnsDigestSizeAndUTCTime(t, newPuller, localImageReference(t))
			assertPullSuccessWithExplicitPlatformAndAuth(t, newPuller, localImageReference(t))
		})
	}
}

func TestImagePullerRealWorldBusybox(t *testing.T) {
	root := filepath.Join(privateTempDir(t), "images")
	puller, err := chamberImagePuller.New(chamberImage.Config{Root: root}, localfs.NewDirectoryManager())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	assertPullSuccessReturnsDigestSizeAndUTCTime(t, func(t *testing.T) (chamberImage.Puller, string) {
		t.Helper()
		return puller, root
	}, imageFixture{
		reference: busyboxReference,
	})
}

func assertPullInvalidReference(t *testing.T, newPuller pullerFactory) {
	t.Helper()

	puller, _ := newPuller(t)

	_, err := puller.Pull(context.Background(), chamberImage.PullRequest{
		Reference: "not a reference !!",
	})
	if err == nil {
		t.Fatal("Pull() error = nil, want invalid reference error")
	}
}

func assertPullFetchFailureLeavesNoFinalLayout(t *testing.T, newPuller pullerFactory) {
	t.Helper()

	registry := testutil.NewFailingRegistry(t)
	reference := registry.Reference(t, "library/busybox", "latest")
	puller, root := newPuller(t)
	destination := imageDestination(t, root, reference)

	_, err := puller.Pull(context.Background(), chamberImage.PullRequest{
		Reference: reference,
	})
	if err == nil {
		t.Fatal("Pull() error = nil, want registry failure")
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("final layout stat error = %v, want %v", statErr, os.ErrNotExist)
	}
}

func assertPullInvalidExistingLayoutIsReturned(t *testing.T, newPuller pullerFactory) {
	t.Helper()

	root := privateTempDir(t)
	image := localImageReference(t)
	destination := imageDestination(t, root, image.reference)
	if err := os.MkdirAll(destination, 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(destination, "existing"), []byte("already here"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	puller, err := chamberImagePuller.New(chamberImage.Config{Root: root}, localfs.NewDirectoryManager())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = puller.Pull(context.Background(), chamberImage.PullRequest{
		Reference: image.reference,
	})
	if err == nil {
		t.Fatal("Pull() error = nil, want invalid existing layout error")
	}
	if _, statErr := os.Stat(filepath.Join(destination, "existing")); statErr != nil {
		t.Fatalf("existing final path changed after rename failure: %v", statErr)
	}
}

func assertPullSuccessReturnsDigestSizeAndUTCTime(t *testing.T, newPuller pullerFactory, image imageFixture) {
	t.Helper()

	puller, root := newPuller(t)
	destination := imageDestination(t, root, image.reference)
	before := time.Now().UTC()

	pulled, err := puller.Pull(context.Background(), chamberImage.PullRequest{
		Reference: image.reference,
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
	assertPullReusesExistingLayout(t, puller, image.reference, pulled)
}

func assertPullSuccessWithExplicitPlatformAndAuth(t *testing.T, newPuller pullerFactory, image imageFixture) {
	t.Helper()

	puller, root := newPuller(t)
	destination := imageDestination(t, root, image.reference)

	pulled, err := puller.Pull(context.Background(), chamberImage.PullRequest{
		Reference: image.reference,
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

func imageDestination(t *testing.T, root string, reference string) string {
	t.Helper()

	destination, err := chamberImage.DestinationForCanonicalReference(root, reference)
	if err != nil {
		t.Fatalf("DestinationForCanonicalReference() error = %v", err)
	}
	return destination
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

func assertPullReusesExistingLayout(t *testing.T, puller chamberImage.Puller, reference string, first chamberImage.PulledImage) {
	t.Helper()

	before := time.Now().UTC()
	reused, err := puller.Pull(context.Background(), chamberImage.PullRequest{
		Reference: reference,
	})
	if err != nil {
		t.Fatalf("Pull(existing layout) error = %v", err)
	}
	after := time.Now().UTC()

	if reused.Reference != first.Reference {
		t.Fatalf("reused Reference = %q, want %q", reused.Reference, first.Reference)
	}
	if reused.Digest != first.Digest {
		t.Fatalf("reused Digest = %q, want %q", reused.Digest, first.Digest)
	}
	if reused.LayoutPath != first.LayoutPath {
		t.Fatalf("reused LayoutPath = %q, want %q", reused.LayoutPath, first.LayoutPath)
	}
	if reused.SizeBytes != first.SizeBytes {
		t.Fatalf("reused SizeBytes = %d, want %d", reused.SizeBytes, first.SizeBytes)
	}
	if reused.PulledAt.Location() != time.UTC {
		t.Fatalf("reused PulledAt location = %v, want UTC", reused.PulledAt.Location())
	}
	if reused.PulledAt.Before(before) || reused.PulledAt.After(after) {
		t.Fatalf("reused PulledAt = %v, want between %v and %v", reused.PulledAt, before, after)
	}
}
