package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ErrRootRequired is returned when image operations receive an empty image root.
var ErrRootRequired = fmt.Errorf("%w: image root is required", chamberErrors.ErrInvalidRequest)

// PullPolicy controls whether a puller may reuse an existing local image
// layout.
type PullPolicy string

const (
	// PullIfMissing reuses an existing layout for the same canonical reference
	// and platform. It is the default when PullRequest.Policy is empty.
	PullIfMissing PullPolicy = "if_missing"

	// PullAlways fetches the reference again and replaces any existing layout at
	// the derived destination. SDK callers are responsible for coordinating
	// concurrent pulls to the same image root.
	PullAlways PullPolicy = "always"
)

// PullRequest describes one image pull into the puller's configured image root.
type PullRequest struct {
	// Reference is the image reference to pull. It may be familiar shorthand such
	// as "alpine:latest"; pullers canonicalize it before deriving storage paths.
	Reference string

	// Platform selects the image platform. Empty OS defaults to linux, empty
	// Architecture defaults to the host Go architecture, and Variant is optional.
	Platform Platform

	// Auth supplies optional registry credentials for this pull.
	Auth *Auth

	// Policy controls reuse of an existing layout. Empty means PullIfMissing.
	Policy PullPolicy
}

// Platform identifies an OCI image platform.
type Platform struct {
	// OS is the target operating system. Empty means linux for Chamber runtime
	// execution.
	OS string

	// Architecture is the target CPU architecture. Empty means runtime.GOARCH.
	Architecture string

	// Variant is the optional architecture variant, such as "v7" for arm.
	Variant string
}

// Auth contains registry authentication material for one pull request.
type Auth struct {
	// Username is used with Password for basic registry authentication.
	Username string

	// Password is used with Username for basic registry authentication.
	Password string

	// Token is used for bearer-token registry authentication.
	Token string
}

// PulledImage describes the OCI image layout produced or reused by a pull.
type PulledImage struct {
	// Reference is the canonical image reference stored in the layout metadata.
	Reference string

	// Digest is the digest returned by the registry for the pulled reference.
	Digest string

	// LayoutPath is the absolute path to the OCI image layout on disk.
	LayoutPath string

	// SizeBytes is the number of bytes reported for the pulled image content
	// when that information is available.
	SizeBytes int64

	// PulledAt records when the puller produced this result.
	PulledAt time.Time
}

// Puller pulls OCI images into a caller-owned image root.
type Puller interface {
	// Pull fetches or reuses the requested image and returns the local OCI image
	// layout. The context controls pull work only; callers own cleanup and
	// coordination for shared roots.
	Pull(ctx context.Context, request PullRequest) (PulledImage, error)
}

// DestinationForCanonicalImage returns the deterministic layout path for a
// canonical image reference and platform.
func DestinationForCanonicalImage(root string, reference string, platform Platform) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", ErrRootRequired
	}
	if strings.TrimSpace(reference) == "" {
		return "", fmt.Errorf("%w: canonical image reference is required", chamberErrors.ErrInvalidRequest)
	}
	identity := reference + "\n" + normalizePlatform(platform)
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(root, hex.EncodeToString(sum[:])), nil
}

func normalizePlatform(platform Platform) string {
	os := strings.TrimSpace(platform.OS)
	if os == "" {
		os = "linux"
	}
	architecture := strings.TrimSpace(platform.Architecture)
	if architecture == "" {
		architecture = goruntime.GOARCH
	}
	variant := strings.TrimSpace(platform.Variant)
	return os + "/" + architecture + "/" + variant
}

// LayoutExists reports whether path is a valid OCI image layout.
func LayoutExists(path string) bool {
	return LayoutExistsContext(context.Background(), path)
}

// LayoutExistsContext reports whether path is a valid OCI image layout,
// returning false when validation fails or ctx is canceled.
func LayoutExistsContext(ctx context.Context, path string) bool {
	return ValidateLayoutContext(ctx, path) == nil
}

// ValidateLayout validates the OCI image layout at path using a background
// context.
func ValidateLayout(path string) error {
	return ValidateLayoutContext(context.Background(), path)
}

// ValidateLayoutContext validates the OCI image layout at path, including
// layout metadata, index descriptors, child manifests, blob presence, blob
// sizes, and blob digests.
func ValidateLayoutContext(ctx context.Context, path string) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: validate OCI image layout canceled before start: %w", chamberErrors.ErrCanceled, err)
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: image layout path is required", chamberErrors.ErrInvalidImageLayout)
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: OCI image layout path does not exist: %q", chamberErrors.ErrInvalidImageLayout, path)
		}
		return fmt.Errorf("%w: stat OCI image layout path %q: %w", chamberErrors.ErrFilesystemFailed, path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: OCI image layout path is not a directory", chamberErrors.ErrInvalidImageLayout)
	}

	layoutFile, err := os.ReadFile(filepath.Join(path, "oci-layout"))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: OCI image layout metadata is missing", chamberErrors.ErrInvalidImageLayout)
		}
		return fmt.Errorf("%w: read OCI image layout metadata: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: validate OCI image layout canceled after reading metadata: %w", chamberErrors.ErrCanceled, err)
	}
	var layoutVersion struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if err := json.Unmarshal(layoutFile, &layoutVersion); err != nil {
		return fmt.Errorf("%w: decode OCI image layout metadata: %w", chamberErrors.ErrInvalidImageLayout, err)
	}
	if layoutVersion.ImageLayoutVersion == "" {
		return fmt.Errorf("%w: OCI image layout version is missing", chamberErrors.ErrInvalidImageLayout)
	}

	indexFile, err := os.ReadFile(filepath.Join(path, "index.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: OCI image layout index is missing", chamberErrors.ErrInvalidImageLayout)
		}
		return fmt.Errorf("%w: read OCI image layout index: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: validate OCI image layout canceled after reading index: %w", chamberErrors.ErrCanceled, err)
	}
	var index imagespec.Index
	if err := json.Unmarshal(indexFile, &index); err != nil {
		return fmt.Errorf("%w: decode OCI image layout index: %w", chamberErrors.ErrInvalidImageLayout, err)
	}
	if len(index.Manifests) == 0 {
		return fmt.Errorf("%w: OCI image layout index has no manifests", chamberErrors.ErrInvalidImageLayout)
	}
	for _, descriptor := range index.Manifests {
		if err := validateLayoutDescriptor(ctx, path, descriptor, true); err != nil {
			return err
		}
	}
	return nil
}

func validateLayoutDescriptor(ctx context.Context, root string, descriptor imagespec.Descriptor, expand bool) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: validate OCI image layout descriptor canceled: %w", chamberErrors.ErrCanceled, err)
	}
	if descriptor.Digest == "" {
		return fmt.Errorf("%w: OCI image layout descriptor is missing digest", chamberErrors.ErrInvalidImageLayout)
	}
	if err := descriptor.Digest.Validate(); err != nil {
		return fmt.Errorf("%w: validate OCI image layout descriptor digest: %w", chamberErrors.ErrInvalidImageLayout, err)
	}
	blobPath := filepath.Join(root, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())
	info, err := os.Stat(blobPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: OCI image layout blob %s is missing", chamberErrors.ErrInvalidImageLayout, descriptor.Digest)
		}
		return fmt.Errorf("%w: stat OCI image layout blob %s: %w", chamberErrors.ErrFilesystemFailed, descriptor.Digest, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%w: OCI image layout blob %s is a directory", chamberErrors.ErrInvalidImageLayout, descriptor.Digest)
	}
	if info.Size() != descriptor.Size {
		return fmt.Errorf("%w: OCI image layout blob %s size = %d, want %d", chamberErrors.ErrInvalidImageLayout, descriptor.Digest, info.Size(), descriptor.Size)
	}
	if err := validateBlobDigest(ctx, blobPath, descriptor); err != nil {
		return err
	}
	if !expand {
		return nil
	}

	switch descriptor.MediaType {
	case imagespec.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		return validateLayoutIndexBlob(ctx, root, blobPath)
	case imagespec.MediaTypeImageManifest, "application/vnd.docker.distribution.manifest.v2+json":
		return validateLayoutManifestBlob(ctx, root, blobPath)
	default:
		return nil
	}
}

func validateBlobDigest(ctx context.Context, path string, descriptor imagespec.Descriptor) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: open OCI image layout blob: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	defer file.Close()

	verifier := descriptor.Digest.Verifier()
	if _, err := io.Copy(verifier, contextReader{ctx: ctx, reader: file}); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: validate OCI image layout blob canceled: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return fmt.Errorf("%w: read OCI image layout blob: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if !verifier.Verified() {
		return fmt.Errorf("%w: OCI image layout blob %s content does not match digest", chamberErrors.ErrInvalidImageLayout, descriptor.Digest)
	}
	return nil
}

func validateLayoutIndexBlob(ctx context.Context, root string, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: read OCI image layout nested index: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: validate OCI image layout nested index canceled: %w", chamberErrors.ErrCanceled, err)
	}
	var index imagespec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return fmt.Errorf("%w: decode OCI image layout nested index: %w", chamberErrors.ErrInvalidImageLayout, err)
	}
	if len(index.Manifests) == 0 {
		return fmt.Errorf("%w: OCI image layout nested index has no manifests", chamberErrors.ErrInvalidImageLayout)
	}
	for _, descriptor := range index.Manifests {
		if err := validateLayoutDescriptor(ctx, root, descriptor, true); err != nil {
			return err
		}
	}
	return nil
}

func validateLayoutManifestBlob(ctx context.Context, root string, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: read OCI image layout manifest: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: validate OCI image layout manifest canceled: %w", chamberErrors.ErrCanceled, err)
	}
	var manifest imagespec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("%w: decode OCI image layout manifest: %w", chamberErrors.ErrInvalidImageLayout, err)
	}
	if err := validateLayoutDescriptor(ctx, root, manifest.Config, false); err != nil {
		return err
	}
	for _, descriptor := range manifest.Layers {
		if err := validateLayoutDescriptor(ctx, root, descriptor, false); err != nil {
			return err
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}
