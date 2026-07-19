package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

var ErrRootRequired = errors.New("image root is required")

type PullRequest struct {
	Reference string
	Platform  Platform
	Auth      *Auth
}

type Platform struct {
	OS           string
	Architecture string
	Variant      string
}

type Auth struct {
	Username string
	Password string
	Token    string
}

type PulledImage struct {
	Reference  string
	Digest     string
	LayoutPath string
	SizeBytes  int64
	PulledAt   time.Time
}

type Puller interface {
	Pull(ctx context.Context, request PullRequest) (PulledImage, error)
}

// DestinationForCanonicalReference returns the deterministic layout path for a
// reference that has already been canonicalized by the image implementation.
func DestinationForCanonicalReference(root string, reference string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", ErrRootRequired
	}
	if strings.TrimSpace(reference) == "" {
		return "", errors.New("canonical image reference is required")
	}
	sum := sha256.Sum256([]byte(reference))
	return filepath.Join(root, hex.EncodeToString(sum[:])), nil
}

func LayoutExists(path string) bool {
	return ValidateLayout(path) == nil
}

func ValidateLayout(path string) error {
	if strings.TrimSpace(path) == "" {
		return ErrRootRequired
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("OCI image layout path is not a directory")
	}

	layoutFile, err := os.ReadFile(filepath.Join(path, "oci-layout"))
	if err != nil {
		return err
	}
	var layoutVersion struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if err := json.Unmarshal(layoutFile, &layoutVersion); err != nil {
		return err
	}
	if layoutVersion.ImageLayoutVersion == "" {
		return errors.New("OCI image layout version is missing")
	}

	indexFile, err := os.ReadFile(filepath.Join(path, "index.json"))
	if err != nil {
		return err
	}
	var index imagespec.Index
	if err := json.Unmarshal(indexFile, &index); err != nil {
		return err
	}
	if len(index.Manifests) == 0 {
		return errors.New("OCI image layout index has no manifests")
	}
	for _, descriptor := range index.Manifests {
		if err := validateLayoutDescriptor(path, descriptor, true); err != nil {
			return err
		}
	}
	return nil
}

func validateLayoutDescriptor(root string, descriptor imagespec.Descriptor, expand bool) error {
	if descriptor.Digest == "" {
		return errors.New("OCI image layout descriptor is missing digest")
	}
	if err := descriptor.Digest.Validate(); err != nil {
		return fmt.Errorf("validate OCI image layout descriptor digest: %w", err)
	}
	blobPath := filepath.Join(root, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())
	info, err := os.Stat(blobPath)
	if err != nil {
		return fmt.Errorf("stat OCI image layout blob %s: %w", descriptor.Digest, err)
	}
	if info.IsDir() {
		return fmt.Errorf("OCI image layout blob %s is a directory", descriptor.Digest)
	}
	if info.Size() != descriptor.Size {
		return fmt.Errorf("OCI image layout blob %s size = %d, want %d", descriptor.Digest, info.Size(), descriptor.Size)
	}
	if err := validateBlobDigest(blobPath, descriptor); err != nil {
		return err
	}
	if !expand {
		return nil
	}

	switch descriptor.MediaType {
	case imagespec.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		return validateLayoutIndexBlob(root, blobPath)
	case imagespec.MediaTypeImageManifest, "application/vnd.docker.distribution.manifest.v2+json":
		return validateLayoutManifestBlob(root, blobPath)
	default:
		return nil
	}
}

func validateBlobDigest(path string, descriptor imagespec.Descriptor) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	verifier := descriptor.Digest.Verifier()
	if _, err := io.Copy(verifier, file); err != nil {
		return err
	}
	if !verifier.Verified() {
		return fmt.Errorf("OCI image layout blob %s content does not match digest", descriptor.Digest)
	}
	return nil
}

func validateLayoutIndexBlob(root string, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var index imagespec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return err
	}
	if len(index.Manifests) == 0 {
		return errors.New("OCI image layout nested index has no manifests")
	}
	for _, descriptor := range index.Manifests {
		if err := validateLayoutDescriptor(root, descriptor, true); err != nil {
			return err
		}
	}
	return nil
}

func validateLayoutManifestBlob(root string, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var manifest imagespec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	if err := validateLayoutDescriptor(root, manifest.Config, false); err != nil {
		return err
	}
	for _, descriptor := range manifest.Layers {
		if err := validateLayoutDescriptor(root, descriptor, false); err != nil {
			return err
		}
	}
	return nil
}
