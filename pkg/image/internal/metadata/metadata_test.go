package metadata

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

func TestJSONMetadataPutGetListAndDelete(t *testing.T) {
	store := newTestMetadata(t)
	img := testImage("example.com/library/app:latest", "sha256:abc123", chamberImage.Platform{OS: "linux", Architecture: "amd64"})

	if err := store.Put(context.Background(), img); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	got, err := store.Get(context.Background(), img.Reference, img.Platform)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Reference != img.Reference || got.Digest != img.Digest || got.Platform.Architecture != "amd64" {
		t.Fatalf("Get() = %#v, want %#v", got, img)
	}

	listed, err := store.List(context.Background(), chamberImage.ListRequest{Reference: img.Reference})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(listed) != 1 || listed[0].Digest != img.Digest {
		t.Fatalf("List() = %#v, want one matching image", listed)
	}

	if err := store.Delete(context.Background(), img.Reference, img.Platform); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(context.Background(), img.Reference, img.Platform); err != nil {
		t.Fatalf("Delete(missing) error = %v, want idempotent success", err)
	}
	if _, err := store.Get(context.Background(), img.Reference, img.Platform); !errors.Is(err, chamberErrors.ErrImageNotFound) {
		t.Fatalf("Get(missing) error = %v, want image not found", err)
	}
}

func TestJSONMetadataRejectsCorruptRecords(t *testing.T) {
	store := newTestMetadata(t)
	img := testImage("example.com/library/app:latest", "sha256:abc123", chamberImage.Platform{})
	path := filepath.Join(store.imagesRoot, Key(img.Reference, img.Platform)+".json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatalf("WriteFile(corrupt record) error = %v", err)
	}

	if _, err := store.Get(context.Background(), img.Reference, img.Platform); !errors.Is(err, chamberErrors.ErrMetadataFailed) {
		t.Fatalf("Get(corrupt) error = %v, want metadata failed", err)
	}
}

func newTestMetadata(t *testing.T) *JSONMetadata {
	t.Helper()

	root := filepath.Join(privateTempDir(t), "metadata")
	workspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    filepath.Dir(root),
		TmpRoot: filepath.Join(t.TempDir(), "tmp"),
		Requirements: hostfs.FeatureSet{
			PrivateDirs:      true,
			FileFsync:        true,
			AtomicFileRename: true,
		},
	})
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}
	store, err := NewJSON(root, workspace)
	if err != nil {
		t.Fatalf("NewJSON() error = %v", err)
	}
	return store
}

func testImage(reference string, imageDigest string, platform chamberImage.Platform) chamberImage.Image {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	return chamberImage.Image{
		Reference: reference,
		Digest:    imageDigest,
		Platform:  chamberImage.NormalizePlatform(platform),
		Source:    chamberImage.SourcePulled,
		SizeBytes: 12,
		CreatedAt: now,
		UpdatedAt: now,
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
