package image_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

type layoutFixture struct {
	path     string
	manifest imagespec.Descriptor
	config   imagespec.Descriptor
	layer    imagespec.Descriptor
}

func TestValidateLayoutRequiresOCILayoutAndManifest(t *testing.T) {
	fixture := writeValidLayout(t)

	if err := chamberImage.ValidateLayout(fixture.path); err != nil {
		t.Fatalf("ValidateLayout(valid) error = %v", err)
	}
	if !chamberImage.LayoutExists(fixture.path) {
		t.Fatal("LayoutExists(valid) = false, want true")
	}
}

func TestValidateLayoutContextHonorsCanceledContext(t *testing.T) {
	fixture := writeValidLayout(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if chamberImage.LayoutExistsContext(ctx, fixture.path) {
		t.Fatal("LayoutExistsContext(canceled) = true, want false")
	}
	if err := chamberImage.ValidateLayoutContext(ctx, fixture.path); err == nil {
		t.Fatal("ValidateLayoutContext(canceled) error = nil, want canceled error")
	} else if !errors.Is(err, chamberErrors.ErrCanceled) {
		t.Fatalf("ValidateLayoutContext(canceled) error = %v, want canceled code", err)
	}
}

func TestValidateLayoutRejectsMissingLayoutFile(t *testing.T) {
	fixture := writeValidLayout(t)
	if err := os.Remove(filepath.Join(fixture.path, "oci-layout")); err != nil {
		t.Fatalf("Remove(oci-layout) error = %v", err)
	}

	if err := chamberImage.ValidateLayout(fixture.path); err == nil {
		t.Fatal("ValidateLayout(missing oci-layout) error = nil, want error")
	} else if !errors.Is(err, chamberErrors.ErrInvalidImageLayout) {
		t.Fatalf("ValidateLayout(missing oci-layout) error = %v, want invalid image layout code", err)
	}
	if chamberImage.LayoutExists(fixture.path) {
		t.Fatal("LayoutExists(missing oci-layout) = true, want false")
	}
}

func TestValidateLayoutRejectsIndexWithoutManifests(t *testing.T) {
	fixture := writeValidLayout(t)
	if err := os.WriteFile(filepath.Join(fixture.path, "index.json"), []byte(`{"schemaVersion":2,"manifests":[]}`), 0600); err != nil {
		t.Fatalf("WriteFile(index.json) error = %v", err)
	}

	if err := chamberImage.ValidateLayout(fixture.path); err == nil {
		t.Fatal("ValidateLayout(empty index) error = nil, want error")
	} else if !errors.Is(err, chamberErrors.ErrInvalidImageLayout) {
		t.Fatalf("ValidateLayout(empty index) error = %v, want invalid image layout code", err)
	}
	if chamberImage.LayoutExists(fixture.path) {
		t.Fatal("LayoutExists(empty index) = true, want false")
	}
}

func TestValidateLayoutRejectsMissingManifestBlob(t *testing.T) {
	fixture := writeValidLayout(t)
	removeBlob(t, fixture.path, fixture.manifest)

	if err := chamberImage.ValidateLayout(fixture.path); err == nil {
		t.Fatal("ValidateLayout(missing manifest blob) error = nil, want error")
	} else if !errors.Is(err, chamberErrors.ErrInvalidImageLayout) {
		t.Fatalf("ValidateLayout(missing manifest blob) error = %v, want invalid image layout code", err)
	}
	if chamberImage.LayoutExists(fixture.path) {
		t.Fatal("LayoutExists(missing manifest blob) = true, want false")
	}
}

func TestValidateLayoutRejectsMissingManifestChildBlob(t *testing.T) {
	fixture := writeValidLayout(t)
	removeBlob(t, fixture.path, fixture.config)

	if err := chamberImage.ValidateLayout(fixture.path); err == nil {
		t.Fatal("ValidateLayout(missing config blob) error = nil, want error")
	} else if !errors.Is(err, chamberErrors.ErrInvalidImageLayout) {
		t.Fatalf("ValidateLayout(missing config blob) error = %v, want invalid image layout code", err)
	}
	if chamberImage.LayoutExists(fixture.path) {
		t.Fatal("LayoutExists(missing config blob) = true, want false")
	}
}

func TestValidateLayoutRejectsManifestBlobWithWrongDigest(t *testing.T) {
	fixture := writeValidLayout(t)
	overwriteBlob(t, fixture.path, fixture.manifest, bytes.Repeat([]byte("x"), int(fixture.manifest.Size)))

	if err := chamberImage.ValidateLayout(fixture.path); err == nil {
		t.Fatal("ValidateLayout(corrupt manifest blob) error = nil, want error")
	} else if !errors.Is(err, chamberErrors.ErrInvalidImageLayout) {
		t.Fatalf("ValidateLayout(corrupt manifest blob) error = %v, want invalid image layout code", err)
	}
	if chamberImage.LayoutExists(fixture.path) {
		t.Fatal("LayoutExists(corrupt manifest blob) = true, want false")
	}
}

func writeValidLayout(t *testing.T) layoutFixture {
	t.Helper()

	path := t.TempDir()
	if err := os.WriteFile(filepath.Join(path, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0600); err != nil {
		t.Fatalf("WriteFile(oci-layout) error = %v", err)
	}

	config := writeBlob(t, path, imagespec.MediaTypeImageConfig, []byte(`{"architecture":"amd64","os":"linux"}`))
	layer := writeBlob(t, path, "application/vnd.oci.image.layer.v1.tar", []byte("layer"))
	manifestBytes := mustJSON(t, imagespec.Manifest{
		Versioned: specsVersioned(),
		Config:    config,
		Layers:    []imagespec.Descriptor{layer},
	})
	manifest := writeBlob(t, path, imagespec.MediaTypeImageManifest, manifestBytes)
	index := mustJSON(t, imagespec.Index{
		Versioned: specsVersioned(),
		Manifests: []imagespec.Descriptor{
			manifest,
		},
	})
	if err := os.WriteFile(filepath.Join(path, "index.json"), index, 0600); err != nil {
		t.Fatalf("WriteFile(index.json) error = %v", err)
	}

	return layoutFixture{
		path:     path,
		manifest: manifest,
		config:   config,
		layer:    layer,
	}
}

func specsVersioned() specs.Versioned {
	return specs.Versioned{
		SchemaVersion: 2,
	}
}

func writeBlob(t *testing.T, root string, mediaType string, content []byte) imagespec.Descriptor {
	t.Helper()

	sum := sha256.Sum256(content)
	encoded := hex.EncodeToString(sum[:])
	path := filepath.Join(root, "blobs", "sha256", encoded)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("MkdirAll(blob parent) error = %v", err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatalf("WriteFile(blob) error = %v", err)
	}
	return imagespec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.Digest("sha256:" + encoded),
		Size:      int64(len(content)),
	}
}

func removeBlob(t *testing.T, root string, descriptor imagespec.Descriptor) {
	t.Helper()

	path := filepath.Join(root, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove(blob) error = %v", err)
	}
}

func overwriteBlob(t *testing.T, root string, descriptor imagespec.Descriptor, content []byte) {
	t.Helper()

	if int64(len(content)) != descriptor.Size {
		t.Fatalf("test corruption content size = %d, want descriptor size %d", len(content), descriptor.Size)
	}
	path := filepath.Join(root, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatalf("WriteFile(blob) error = %v", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return data
}
