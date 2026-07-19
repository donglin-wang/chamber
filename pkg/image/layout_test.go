package image_test

import (
	"os"
	"path/filepath"
	"testing"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
)

const manifestDigest = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"

func TestValidateLayoutRequiresOCILayoutAndManifest(t *testing.T) {
	path := writeValidLayout(t)

	if err := chamberImage.ValidateLayout(path); err != nil {
		t.Fatalf("ValidateLayout(valid) error = %v", err)
	}
	if !chamberImage.LayoutExists(path) {
		t.Fatal("LayoutExists(valid) = false, want true")
	}
}

func TestValidateLayoutRejectsMissingLayoutFile(t *testing.T) {
	path := writeValidLayout(t)
	if err := os.Remove(filepath.Join(path, "oci-layout")); err != nil {
		t.Fatalf("Remove(oci-layout) error = %v", err)
	}

	if err := chamberImage.ValidateLayout(path); err == nil {
		t.Fatal("ValidateLayout(missing oci-layout) error = nil, want error")
	}
	if chamberImage.LayoutExists(path) {
		t.Fatal("LayoutExists(missing oci-layout) = true, want false")
	}
}

func TestValidateLayoutRejectsIndexWithoutManifests(t *testing.T) {
	path := writeValidLayout(t)
	if err := os.WriteFile(filepath.Join(path, "index.json"), []byte(`{"schemaVersion":2,"manifests":[]}`), 0600); err != nil {
		t.Fatalf("WriteFile(index.json) error = %v", err)
	}

	if err := chamberImage.ValidateLayout(path); err == nil {
		t.Fatal("ValidateLayout(empty index) error = nil, want error")
	}
	if chamberImage.LayoutExists(path) {
		t.Fatal("LayoutExists(empty index) = true, want false")
	}
}

func writeValidLayout(t *testing.T) string {
	t.Helper()

	path := t.TempDir()
	if err := os.WriteFile(filepath.Join(path, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0600); err != nil {
		t.Fatalf("WriteFile(oci-layout) error = %v", err)
	}
	index := `{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + manifestDigest + `","size":2}]}`
	if err := os.WriteFile(filepath.Join(path, "index.json"), []byte(index), 0600); err != nil {
		t.Fatalf("WriteFile(index.json) error = %v", err)
	}
	return path
}
