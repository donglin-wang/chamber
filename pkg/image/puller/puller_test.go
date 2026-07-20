package puller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	chamberImageShared "github.com/donglin-wang/chamber/pkg/image/shared"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	"github.com/donglin-wang/chamber/pkg/shared/testutil"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

const busyboxReference = "index.docker.io/library/busybox:latest"

func TestPullRejectsUnsupportedPolicy(t *testing.T) {
	puller, err := New(chamberImageShared.Config{Root: filepath.Join(privateTempDir(t), "images")}, localfs.NewDirectoryManager())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = puller.Pull(context.Background(), chamberImageShared.PullRequest{
		Reference: "example.com/library/busybox:latest",
		Policy:    chamberImageShared.PullPolicy("eventually"),
	})
	if err == nil {
		t.Fatal("Pull() error = nil, want unsupported policy error")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("Pull() error = %v, want invalid request code", err)
	}
}

func TestResolvePlatformDefaultsToLinuxHostArchitecture(t *testing.T) {
	platform := resolvePlatform(chamberImageShared.Platform{})

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

func privateTempDir(t *testing.T) string {
	t.Helper()

	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatalf("Chmod(%q) error = %v", path, err)
	}
	return path
}

func TestResolvePlatformAppliesRequestFields(t *testing.T) {
	platform := resolvePlatform(chamberImageShared.Platform{
		OS:           "linux",
		Architecture: "arm64",
		Variant:      "v8",
	})

	if platform.OS != "linux" || platform.Architecture != "arm64" || platform.Variant != "v8" {
		t.Fatalf("platform = %#v, want linux/arm64/v8", platform)
	}
}

func TestAuthenticatorAppliesBasicAndTokenAuth(t *testing.T) {
	auth, err := authenticator(&chamberImageShared.Auth{
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

func TestExistingPulledImageRequiresMatchingReferenceAnnotation(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "layout")
	layoutPath, err := layout.Write(path, empty.Index)
	if err != nil {
		t.Fatalf("layout.Write() error = %v", err)
	}
	if err := layoutPath.AppendImage(empty.Image); err != nil {
		t.Fatalf("AppendImage() error = %v", err)
	}

	_, err = existingPulledImage("example.com/library/busybox:latest", resolvePlatform(chamberImageShared.Platform{}), path)
	if err == nil {
		t.Fatal("existingPulledImage() error = nil, want missing reference error")
	}
	if !strings.Contains(err.Error(), "no manifest for reference") {
		t.Fatalf("existingPulledImage() error = %v, want missing reference error", err)
	}
}

func TestImagePullerRealWorldBusybox(t *testing.T) {
	puller, root := newTestPuller(t)
	assertPullSuccessReturnsDigestSizeAndUTCTime(t, puller, root, imageFixture{
		reference: busyboxReference,
	})
}

func TestPullInvalidReference(t *testing.T) {
	puller, _ := newTestPuller(t)

	_, err := puller.Pull(context.Background(), chamberImageShared.PullRequest{
		Reference: "not a reference !!",
	})
	if err == nil {
		t.Fatal("Pull() error = nil, want invalid reference error")
	}
}

func TestPullFetchFailureLeavesNoFinalLayout(t *testing.T) {
	registry := testutil.NewFailingRegistry(t)
	reference := registry.Reference(t, "library/busybox", "latest")
	puller, root := newTestPuller(t)
	destination := imageDestination(t, root, reference)

	_, err := puller.Pull(context.Background(), chamberImageShared.PullRequest{
		Reference: reference,
	})
	if err == nil {
		t.Fatal("Pull() error = nil, want registry failure")
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("final layout stat error = %v, want %v", statErr, os.ErrNotExist)
	}
}

func TestPullInvalidExistingLayoutIsReturned(t *testing.T) {
	root := privateTempDir(t)
	image := localImageReference(t)
	destination := imageDestination(t, root, image.reference)
	if err := os.MkdirAll(destination, 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(destination, "existing"), []byte("already here"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	puller, err := New(chamberImageShared.Config{Root: root}, localfs.NewDirectoryManager())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = puller.Pull(context.Background(), chamberImageShared.PullRequest{
		Reference: image.reference,
	})
	if err == nil {
		t.Fatal("Pull() error = nil, want invalid existing layout error")
	}
	if _, statErr := os.Stat(filepath.Join(destination, "existing")); statErr != nil {
		t.Fatalf("existing final path changed after rename failure: %v", statErr)
	}
}

func TestPullSuccessReturnsDigestSizeAndUTCTime(t *testing.T) {
	puller, root := newTestPuller(t)
	assertPullSuccessReturnsDigestSizeAndUTCTime(t, puller, root, localImageReference(t))
}

func assertPullSuccessReturnsDigestSizeAndUTCTime(t *testing.T, puller chamberImageShared.Puller, root string, image imageFixture) {
	t.Helper()

	destination := imageDestination(t, root, image.reference)
	before := time.Now().UTC()

	pulled, err := puller.Pull(context.Background(), chamberImageShared.PullRequest{
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

func TestPullSuccessWithExplicitPlatformAndAuth(t *testing.T) {
	image := localImageReference(t)
	puller, root := newTestPuller(t)
	destination := imageDestination(t, root, image.reference)

	pulled, err := puller.Pull(context.Background(), chamberImageShared.PullRequest{
		Reference: image.reference,
		Platform: chamberImageShared.Platform{
			OS:           "linux",
			Architecture: runtime.GOARCH,
		},
		Auth: &chamberImageShared.Auth{
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

func TestPullAlwaysRefreshesMutableTag(t *testing.T) {
	registry := testutil.NewFakeRegistry(t)
	reference, digest := registry.PushRandomImage(t, "library/mutable", "latest")
	puller, _ := newTestPuller(t)
	first, err := puller.Pull(context.Background(), chamberImageShared.PullRequest{
		Reference: reference,
	})
	if err != nil {
		t.Fatalf("Pull(initial) error = %v", err)
	}
	if first.Digest != digest.String() {
		t.Fatalf("initial Digest = %q, want %q", first.Digest, digest)
	}

	_, refreshedDigest := registry.PushRandomImage(t, "library/mutable", "latest")
	if refreshedDigest.String() == first.Digest {
		t.Fatalf("test registry generated same digest twice: %s", refreshedDigest)
	}
	cached, err := puller.Pull(context.Background(), chamberImageShared.PullRequest{
		Reference: reference,
	})
	if err != nil {
		t.Fatalf("Pull(cached) error = %v", err)
	}
	if cached.Digest != first.Digest {
		t.Fatalf("cached Digest = %q, want original %q", cached.Digest, first.Digest)
	}

	refreshed, err := puller.Pull(context.Background(), chamberImageShared.PullRequest{
		Reference: reference,
		Policy:    chamberImageShared.PullAlways,
	})
	if err != nil {
		t.Fatalf("Pull(always) error = %v", err)
	}
	if refreshed.Digest != refreshedDigest.String() {
		t.Fatalf("refreshed Digest = %q, want %q", refreshed.Digest, refreshedDigest)
	}
}

type imageFixture struct {
	reference string
	digest    string
}

func newTestPuller(t *testing.T) (chamberImageShared.Puller, string) {
	t.Helper()

	root := filepath.Join(privateTempDir(t), "images")
	puller, err := New(chamberImageShared.Config{Root: root}, localfs.NewDirectoryManager())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return puller, root
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

func imageDestination(t *testing.T, root string, reference string) string {
	t.Helper()

	destination, err := chamberImageShared.DestinationForCanonicalImage(root, reference, chamberImageShared.Platform{})
	if err != nil {
		t.Fatalf("DestinationForCanonicalImage() error = %v", err)
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

func assertPullReusesExistingLayout(t *testing.T, puller chamberImageShared.Puller, reference string, first chamberImageShared.PulledImage) {
	t.Helper()

	before := time.Now().UTC()
	reused, err := puller.Pull(context.Background(), chamberImageShared.PullRequest{
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
