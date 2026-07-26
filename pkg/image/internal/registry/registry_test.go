package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/testutil"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

const busyboxReference = "index.docker.io/library/busybox:latest"

func TestPullWritesOCIImageLayoutWithReferenceAnnotation(t *testing.T) {
	registry := testutil.NewFakeRegistry(t)
	reference, _ := registry.PushRandomImage(t, "library/app", "latest")
	destination := filepath.Join(privateTempDir(t), "layout")

	if err := Pull(context.Background(), chamberImage.PullRequest{Reference: reference}, reference, destination); err != nil {
		t.Fatalf("Pull() error = %v", err)
	}

	if !chamberImage.LayoutExists(destination) {
		t.Fatalf("LayoutExists(%q) = false, want valid OCI layout", destination)
	}
	assertLayoutHasImageRef(t, destination, reference)
}

func TestPullFetchFailureLeavesNoDestinationLayout(t *testing.T) {
	registry := testutil.NewFailingRegistry(t)
	reference := registry.Reference(t, "library/app", "latest")
	destination := filepath.Join(privateTempDir(t), "layout")

	err := Pull(context.Background(), chamberImage.PullRequest{Reference: reference}, reference, destination)
	if err == nil {
		t.Fatal("Pull() error = nil, want registry failure")
	}
	if !errors.Is(err, chamberErrors.ErrPullFailed) {
		t.Fatalf("Pull() error = %v, want pull failed code", err)
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("destination stat error = %v, want %v", statErr, os.ErrNotExist)
	}
}

func TestPullRejectsInvalidCanonicalReference(t *testing.T) {
	err := Pull(context.Background(), chamberImage.PullRequest{}, "not a reference !!", filepath.Join(privateTempDir(t), "layout"))
	if err == nil {
		t.Fatal("Pull() error = nil, want invalid reference")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidImageReference) {
		t.Fatalf("Pull() error = %v, want invalid image reference code", err)
	}
}

func TestGeneratedLayoutValidationErrorPreservesCancellationCode(t *testing.T) {
	err := generatedLayoutValidationError(chamberErrors.ErrCanceled)
	if !errors.Is(err, chamberErrors.ErrCanceled) {
		t.Fatalf("generatedLayoutValidationError() error = %v, want canceled code", err)
	}
	if errors.Is(err, chamberErrors.ErrPullFailed) {
		t.Fatalf("generatedLayoutValidationError() error = %v, should not include pull failed code", err)
	}
}

func TestResolvePlatformDefaultsToLinuxHostArchitecture(t *testing.T) {
	platform := ResolvePlatform(chamberImage.Platform{})

	if platform.OS != "linux" {
		t.Fatalf("OS = %q, want linux", platform.OS)
	}
	if platform.Architecture != runtime.GOARCH {
		t.Fatalf("Architecture = %q, want %q", platform.Architecture, runtime.GOARCH)
	}
	if platform.Variant != "" {
		t.Fatalf("Variant = %q, want empty", platform.Variant)
	}
}

func TestResolvePlatformAppliesRequestFields(t *testing.T) {
	platform := ResolvePlatform(chamberImage.Platform{
		OS:           "linux",
		Architecture: "arm64",
		Variant:      "v8",
	})

	if platform.OS != "linux" || platform.Architecture != "arm64" || platform.Variant != "v8" {
		t.Fatalf("platform = %#v, want linux/arm64/v8", platform)
	}
}

func TestAuthenticatorAppliesBasicAndTokenAuth(t *testing.T) {
	auth, err := authenticator(&chamberImage.Auth{
		Username: "user",
		Password: "pass",
		Token:    "registry-token",
	}).Authorization()
	if err != nil {
		t.Fatalf("Authorization() error = %v", err)
	}

	if auth.Username != "user" || auth.Password != "pass" || auth.RegistryToken != "registry-token" {
		t.Fatalf("auth config = %#v, want username/password/token", auth)
	}
}

func TestRegistryRealWorldBusybox(t *testing.T) {
	if os.Getenv("CHAMBER_INTEGRATION") != "1" {
		t.Skip("set CHAMBER_INTEGRATION=1 to run registry integration tests")
	}

	destination := filepath.Join(privateTempDir(t), "layout")
	if err := Pull(context.Background(), chamberImage.PullRequest{Reference: busyboxReference}, busyboxReference, destination); err != nil {
		t.Fatalf("Pull(busybox) error = %v", err)
	}
	assertLayoutHasImageRef(t, destination, busyboxReference)
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

func privateTempDir(t *testing.T) string {
	t.Helper()

	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatalf("Chmod(%q) error = %v", path, err)
	}
	return path
}
