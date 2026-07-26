package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	"github.com/donglin-wang/chamber/pkg/shared/testutil"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

const busyboxReference = "index.docker.io/library/busybox:latest"

func TestStorePullCommitsToSharedLayoutAndReusesMetadata(t *testing.T) {
	store, root := newTestStore(t, chamberImage.Config{})
	registry := testutil.NewFakeRegistry(t)
	reference, expectedDigest := registry.PushRandomImage(t, "library/app", "latest")

	before := time.Now().UTC()
	img, err := store.Pull(context.Background(), chamberImage.PullRequest{Reference: reference})
	if err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	after := time.Now().UTC()
	if img.Reference != reference {
		t.Fatalf("Reference = %q, want %q", img.Reference, reference)
	}
	if img.Digest != expectedDigest.String() {
		t.Fatalf("Digest = %q, want %q", img.Digest, expectedDigest)
	}
	if img.Source != chamberImage.SourcePulled {
		t.Fatalf("Source = %q, want pulled", img.Source)
	}
	if img.CreatedAt.Before(before) || img.CreatedAt.After(after) || img.UpdatedAt.Before(before) || img.UpdatedAt.After(after) {
		t.Fatalf("timestamps = %v/%v, want between %v and %v", img.CreatedAt, img.UpdatedAt, before, after)
	}
	layoutPath, err := store.Layout(context.Background())
	if err != nil {
		t.Fatalf("Layout() error = %v", err)
	}
	if layoutPath != filepath.Join(root, "layout") {
		t.Fatalf("Layout() = %q, want shared layout under root", layoutPath)
	}
	assertSharedLayoutHasDescriptor(t, layoutPath, reference, img.Digest)

	cached, err := store.Pull(context.Background(), chamberImage.PullRequest{Reference: reference})
	if err != nil {
		t.Fatalf("Pull(cached) error = %v", err)
	}
	if !cached.CreatedAt.Equal(img.CreatedAt) || !cached.UpdatedAt.Equal(img.UpdatedAt) {
		t.Fatalf("cached timestamps changed: got %v/%v want %v/%v", cached.CreatedAt, cached.UpdatedAt, img.CreatedAt, img.UpdatedAt)
	}
}

func TestStorePullAlwaysRefreshesSharedLayoutReference(t *testing.T) {
	store, _ := newTestStore(t, chamberImage.Config{})
	registry := testutil.NewFakeRegistry(t)
	reference, firstDigest := registry.PushRandomImage(t, "library/mutable", "latest")
	first, err := store.Pull(context.Background(), chamberImage.PullRequest{Reference: reference})
	if err != nil {
		t.Fatalf("Pull(initial) error = %v", err)
	}
	if first.Digest != firstDigest.String() {
		t.Fatalf("initial Digest = %q, want %q", first.Digest, firstDigest)
	}

	_, refreshedDigest := registry.PushRandomImage(t, "library/mutable", "latest")
	refreshed, err := store.Pull(context.Background(), chamberImage.PullRequest{
		Reference: reference,
		Policy:    chamberImage.PullAlways,
	})
	if err != nil {
		t.Fatalf("Pull(always) error = %v", err)
	}
	if refreshed.Digest != refreshedDigest.String() {
		t.Fatalf("refreshed Digest = %q, want %q", refreshed.Digest, refreshedDigest)
	}
	if !refreshed.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("CreatedAt = %v, want preserved %v", refreshed.CreatedAt, first.CreatedAt)
	}
	if !refreshed.UpdatedAt.After(first.UpdatedAt) && !refreshed.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("UpdatedAt = %v, want refreshed at or after %v", refreshed.UpdatedAt, first.UpdatedAt)
	}
	layoutPath, err := store.Layout(context.Background())
	if err != nil {
		t.Fatalf("Layout() error = %v", err)
	}
	assertSharedLayoutHasDescriptor(t, layoutPath, reference, refreshed.Digest)
	assertSharedLayoutDescriptorCount(t, layoutPath, reference, 1)
}

func TestStoreRemoveDeletesMetadataAndIndexReferenceButLeavesLayout(t *testing.T) {
	store, _ := newTestStore(t, chamberImage.Config{})
	registry := testutil.NewFakeRegistry(t)
	reference, _ := registry.PushRandomImage(t, "library/app", "latest")
	img, err := store.Pull(context.Background(), chamberImage.PullRequest{Reference: reference})
	if err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	layoutPath, err := store.Layout(context.Background())
	if err != nil {
		t.Fatalf("Layout() error = %v", err)
	}

	if err := store.Remove(context.Background(), chamberImage.RemoveRequest{Reference: reference, Platform: img.Platform}); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := store.Remove(context.Background(), chamberImage.RemoveRequest{Reference: reference, Platform: img.Platform}); err != nil {
		t.Fatalf("Remove(missing) error = %v, want idempotent success", err)
	}
	images, err := store.List(context.Background(), chamberImage.ListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("List() = %#v, want empty", images)
	}
	if err := validateStoreLayout(layoutPath); err != nil {
		t.Fatalf("shared store layout invalid after logical remove: %v", err)
	}
	assertSharedLayoutDescriptorCount(t, layoutPath, reference, 0)
}

func TestStoreBuildValidatesContextPath(t *testing.T) {
	store, _ := newTestStore(t, chamberImage.Config{})

	_, err := store.Build(context.Background(), chamberImage.BuildRequest{
		Reference:   "example.com/library/app:latest",
		ContextPath: filepath.Join(privateTempDir(t), "missing"),
	})
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("Build() error = %v, want invalid request", err)
	}
}

func TestStoreBuildahBuilderLazyInitializesOnce(t *testing.T) {
	workerPath := filepath.Join(privateTempDir(t), "buildah-worker")
	if err := os.WriteFile(workerPath, []byte("#!/bin/sh\nprintf '{\"protocol_version\":1}\\n'\n"), 0700); err != nil {
		t.Fatalf("WriteFile(buildah-worker) error = %v", err)
	}
	store, _ := newTestStore(t, chamberImage.Config{
		Buildah: chamberImage.BuildahConfig{Path: workerPath},
	})
	if store.builder != nil {
		t.Fatal("builder initialized before first buildahBuilder call")
	}

	first, err := store.buildahBuilder(context.Background())
	if err != nil {
		t.Fatalf("buildahBuilder(first) error = %v", err)
	}
	second, err := store.buildahBuilder(context.Background())
	if err != nil {
		t.Fatalf("buildahBuilder(second) error = %v", err)
	}
	if first != second {
		t.Fatal("buildahBuilder returned different builder pointers")
	}
}

func TestStoreBuildahBuilderRetriesAfterInitializationFailure(t *testing.T) {
	store, root := newTestStore(t, chamberImage.Config{
		Buildah: chamberImage.BuildahConfig{Path: filepath.Join(privateTempDir(t), "missing-buildah-worker")},
	})

	if _, err := store.buildahBuilder(context.Background()); !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("buildahBuilder(missing worker) error = %v, want invalid request", err)
	}
	if store.builder != nil {
		t.Fatal("builder cached after failed initialization")
	}

	workerPath := filepath.Join(root, "buildah-worker")
	if err := os.WriteFile(workerPath, []byte("#!/bin/sh\nprintf '{\"protocol_version\":1}\\n'\n"), 0700); err != nil {
		t.Fatalf("WriteFile(buildah-worker) error = %v", err)
	}
	store.config.Buildah.Path = workerPath

	builder, err := store.buildahBuilder(context.Background())
	if err != nil {
		t.Fatalf("buildahBuilder(retry) error = %v", err)
	}
	if builder == nil || store.builder != builder {
		t.Fatal("builder was not cached after successful retry")
	}
}

func TestStoreRealWorldBusybox(t *testing.T) {
	if os.Getenv("CHAMBER_INTEGRATION") != "1" {
		t.Skip("set CHAMBER_INTEGRATION=1 to run registry integration tests")
	}

	store, _ := newTestStore(t, chamberImage.Config{})
	img, err := store.Pull(context.Background(), chamberImage.PullRequest{Reference: busyboxReference})
	if err != nil {
		t.Fatalf("Pull(busybox) error = %v", err)
	}
	if img.Reference != busyboxReference {
		t.Fatalf("Reference = %q, want %q", img.Reference, busyboxReference)
	}
	if img.Digest == "" {
		t.Fatal("Digest = empty, want resolved digest")
	}
}

func TestStoreRealBuildahDockerfile(t *testing.T) {
	if os.Getenv("CHAMBER_INTEGRATION") != "1" {
		t.Skip("set CHAMBER_INTEGRATION=1 to run Buildah integration tests")
	}

	contextRoot := privateTempDir(t)
	if err := os.WriteFile(filepath.Join(contextRoot, "Dockerfile"), []byte("FROM scratch\nCOPY hello.txt /hello.txt\n"), 0600); err != nil {
		t.Fatalf("WriteFile(Dockerfile) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(contextRoot, "hello.txt"), []byte("hello from buildah\n"), 0600); err != nil {
		t.Fatalf("WriteFile(hello.txt) error = %v", err)
	}

	store, root := newTestStore(t, chamberImage.Config{
		Buildah: chamberImage.BuildahConfig{
			Path: buildBuildahWorker(t),
		},
	})
	img, err := store.Build(context.Background(), chamberImage.BuildRequest{
		Reference:   "example.com/chamber/buildah-e2e:latest",
		ContextPath: contextRoot,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if img.Source != chamberImage.SourceBuilt {
		t.Fatalf("Source = %q, want built", img.Source)
	}
	if img.Digest == "" {
		t.Fatal("Digest = empty, want generated image digest")
	}
	layoutPath, err := store.Layout(context.Background())
	if err != nil {
		t.Fatalf("Layout() error = %v", err)
	}
	if layoutPath != filepath.Join(root, "layout") {
		t.Fatalf("Layout() = %q, want shared layout below root", layoutPath)
	}
	assertSharedLayoutHasDescriptor(t, layoutPath, img.Reference, img.Digest)
}

func TestStoreRealBuildahDockerfileWithRun(t *testing.T) {
	if os.Getenv("CHAMBER_INTEGRATION") != "1" {
		t.Skip("set CHAMBER_INTEGRATION=1 to run Buildah integration tests")
	}

	contextRoot := privateTempDir(t)
	if err := os.WriteFile(filepath.Join(contextRoot, "Dockerfile"), []byte("FROM docker.io/library/busybox:latest\nRUN echo hello-from-run > /hello.txt\n"), 0600); err != nil {
		t.Fatalf("WriteFile(Dockerfile) error = %v", err)
	}

	store, _ := newTestStore(t, chamberImage.Config{
		Buildah: chamberImage.BuildahConfig{
			Path: buildBuildahWorker(t),
		},
	})
	img, err := store.Build(context.Background(), chamberImage.BuildRequest{
		Reference:   "example.com/chamber/buildah-run-e2e:latest",
		ContextPath: contextRoot,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if img.Source != chamberImage.SourceBuilt {
		t.Fatalf("Source = %q, want built", img.Source)
	}
	if img.Digest == "" {
		t.Fatal("Digest = empty, want generated image digest")
	}
}

func newTestStore(t *testing.T, cfg chamberImage.Config) (*Store, string) {
	t.Helper()

	root := filepath.Join(privateTempDir(t), "images")
	cfg.Root = root
	store, err := New(cfg, localfs.NewDirectoryManager())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return store, root
}

func assertSharedLayoutHasDescriptor(t *testing.T, path string, reference string, imageDigest string) {
	t.Helper()

	if !chamberImage.LayoutExists(path) {
		t.Fatalf("LayoutExists(%q) = false, want true", path)
	}
	index := readTestIndex(t, path)
	for _, descriptor := range index.Manifests {
		if descriptor.Annotations[imagespec.AnnotationRefName] == reference && descriptor.Digest.String() == imageDigest {
			return
		}
	}
	t.Fatalf("shared layout missing descriptor for reference %q digest %q", reference, imageDigest)
}

func assertSharedLayoutDescriptorCount(t *testing.T, path string, reference string, count int) {
	t.Helper()

	index := readTestIndex(t, path)
	actual := 0
	for _, descriptor := range index.Manifests {
		if descriptor.Annotations[imagespec.AnnotationRefName] == reference {
			actual++
		}
	}
	if actual != count {
		t.Fatalf("descriptor count for %q = %d, want %d", reference, actual, count)
	}
}

func readTestIndex(t *testing.T, path string) imagespec.Index {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(path, "index.json"))
	if err != nil {
		t.Fatalf("ReadFile(index.json) error = %v", err)
	}
	var specIndex imagespec.Index
	if err := json.Unmarshal(data, &specIndex); err != nil {
		t.Fatalf("Unmarshal(index manifest) error = %v", err)
	}
	return specIndex
}

func privateTempDir(t *testing.T) string {
	t.Helper()

	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatalf("Chmod(%q) error = %v", path, err)
	}
	return path
}

func buildBuildahWorker(t *testing.T) string {
	t.Helper()

	workerPath := filepath.Join(privateTempDir(t), "buildah-worker")
	command := exec.Command("go", "build", "-p", "1", "-tags", "containers_image_openpgp exclude_graphdriver_btrfs", "-o", workerPath, "github.com/donglin-wang/chamber/cmd/buildah-worker")
	command.Env = append(os.Environ(), "CGO_ENABLED=1", "GOMAXPROCS=2")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go build buildah-worker error = %v\n%s", err, string(output))
	}
	return workerPath
}
