// Package store provides Chamber's filesystem-backed image store.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	"github.com/donglin-wang/chamber/pkg/image/internal/buildkit"
	imageMetadata "github.com/donglin-wang/chamber/pkg/image/internal/metadata"
	"github.com/donglin-wang/chamber/pkg/image/internal/registry"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

type Store struct {
	config     chamberImage.Config
	workspace  *hostfs.Workspace
	metadata   imageMetadata.Metadata
	layoutRoot string
	tmpRoot    string
	logger     *chamberLogging.SlogLogger
	mu         sync.Mutex
	builderMu  sync.Mutex
	buildMu    sync.Mutex
	builder    *buildkit.Builder
}

var _ chamberImage.Store = (*Store)(nil)

func New(config chamberImage.Config, workspace *hostfs.Workspace) (*Store, error) {
	if workspace == nil {
		return nil, fmt.Errorf("%w: image workspace is required", chamberErrors.ErrInvalidRequest)
	}
	if strings.TrimSpace(config.Root) == "" {
		return nil, chamberImage.ErrRootRequired
	}
	if err := requireWorkspaceRoot("image root", config.Root, workspace.Root()); err != nil {
		return nil, err
	}
	if err := requireWorkspaceCapabilities("image workspace", workspace.Capabilities(), hostfs.Capabilities{
		PrivateDirs:           true,
		FileFsync:             true,
		AtomicFileRename:      true,
		AtomicDirectoryRename: true,
	}); err != nil {
		return nil, err
	}
	config.Root = workspace.Root()
	logger, err := chamberLogging.LoggerFromConfig(config.Logging, nil)
	if err != nil {
		return nil, err
	}

	layoutRoot := SharedLayoutDirectory(config.Root)
	metadataRoot := filepath.Join(config.Root, "metadata")
	tmpRoot := workspace.TmpRoot()
	for _, rel := range []string{"layout", "metadata"} {
		if _, err := workspace.MkdirPrivate(rel); err != nil {
			return nil, fmt.Errorf("%w: create image store directory %q: %w", chamberErrors.ErrFilesystemFailed, rel, err)
		}
	}
	if err := initializeStoreLayout(workspace); err != nil {
		return nil, err
	}
	metadataStore, err := imageMetadata.NewJSON(metadataRoot, workspace)
	if err != nil {
		return nil, err
	}

	return &Store{
		config:     config,
		workspace:  workspace,
		metadata:   metadataStore,
		layoutRoot: layoutRoot,
		tmpRoot:    tmpRoot,
		logger:     logger,
	}, nil
}

func SharedLayoutDirectory(root string) string {
	return filepath.Join(root, "layout")
}

func requireWorkspaceRoot(label string, configured string, actual string) error {
	configuredAbs, err := filepath.Abs(configured)
	if err != nil {
		return fmt.Errorf("%w: resolve %s %q: %w", chamberErrors.ErrInvalidRequest, label, configured, err)
	}
	if filepath.Clean(configuredAbs) != filepath.Clean(actual) {
		return fmt.Errorf("%w: %s %q does not match workspace root %q", chamberErrors.ErrInvalidRequest, label, configured, actual)
	}
	return nil
}

func requireWorkspaceCapabilities(label string, observed hostfs.Capabilities, required hostfs.Capabilities) error {
	if required.PrivateDirs && !observed.PrivateDirs {
		return fmt.Errorf("%w: %s requires private directories", chamberErrors.ErrFilesystemFailed, label)
	}
	if required.FileFsync && !observed.FileFsync {
		return fmt.Errorf("%w: %s requires file fsync", chamberErrors.ErrFilesystemFailed, label)
	}
	if required.DirectoryFsync && !observed.DirectoryFsync {
		return fmt.Errorf("%w: %s requires directory fsync", chamberErrors.ErrFilesystemFailed, label)
	}
	if required.AtomicFileRename && !observed.AtomicFileRename {
		return fmt.Errorf("%w: %s requires atomic file rename between temporary and durable roots", chamberErrors.ErrFilesystemFailed, label)
	}
	if required.AtomicDirectoryRename && !observed.AtomicDirectoryRename {
		return fmt.Errorf("%w: %s requires atomic directory rename between temporary and durable roots", chamberErrors.ErrFilesystemFailed, label)
	}
	return nil
}

func (s *Store) Pull(ctx context.Context, request chamberImage.PullRequest) (chamberImage.Image, error) {
	canonicalReference, platform, policy, err := resolvePullRequest(ctx, request)
	if err != nil {
		return chamberImage.Image{}, err
	}
	if err := s.requireReady(); err != nil {
		return chamberImage.Image{}, err
	}

	if policy == chamberImage.PullIfMissing {
		s.mu.Lock()
		existing, err := s.existingValidImage(ctx, canonicalReference, platform)
		s.mu.Unlock()
		if err == nil {
			chamberLogging.InfoWith(s.logger, ctx, "reused image",
				"image_ref", existing.Reference,
				"digest", existing.Digest,
				"size_bytes", existing.SizeBytes,
			)
			return existing, nil
		}
		if errors.Is(err, chamberErrors.ErrInvalidImageLayout) {
			return chamberImage.Image{}, err
		}
	}

	tmp, err := s.workspace.MkdirTemp(".", ".pull-*")
	if err != nil {
		return chamberImage.Image{}, fmt.Errorf("%w: create temporary image layout: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	defer os.RemoveAll(tmp)

	chamberLogging.InfoWith(s.logger, ctx, "pulling image",
		"image_ref", canonicalReference,
		"layout_path", s.layoutRoot,
		"platform_os", platform.OS,
		"platform_architecture", platform.Architecture,
		"platform_variant", platform.Variant,
	)
	if err := registry.Pull(ctx, request, canonicalReference, tmp); err != nil {
		return chamberImage.Image{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if policy == chamberImage.PullIfMissing {
		existing, err := s.existingValidImage(ctx, canonicalReference, platform)
		if err == nil {
			return existing, nil
		}
		if errors.Is(err, chamberErrors.ErrInvalidImageLayout) {
			return chamberImage.Image{}, err
		}
	}
	return s.commitLayout(ctx, commitRequest{
		tempLayout:          tmp,
		canonicalReference:  canonicalReference,
		platform:            platform,
		source:              chamberImage.SourcePulled,
		preserveCreatedTime: true,
	})
}

func (s *Store) Build(ctx context.Context, request chamberImage.BuildRequest) (chamberImage.Image, error) {
	canonicalReference, err := chamberImage.CanonicalImageReference(request.Reference)
	if err != nil {
		return chamberImage.Image{}, err
	}
	platform := chamberImage.NormalizePlatform(request.Platform)
	if err := s.requireReady(); err != nil {
		return chamberImage.Image{}, err
	}
	if err := buildkit.ValidateRequest(request); err != nil {
		return chamberImage.Image{}, err
	}

	builder, err := s.buildkitBuilder(ctx)
	if err != nil {
		return chamberImage.Image{}, err
	}

	tmp, err := s.workspace.MkdirTemp(".", ".build-*")
	if err != nil {
		return chamberImage.Image{}, fmt.Errorf("%w: create temporary build layout: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	defer os.RemoveAll(tmp)

	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	if err := builder.Build(ctx, request, tmp); err != nil {
		return chamberImage.Image{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLayout(ctx, commitRequest{
		tempLayout:          tmp,
		canonicalReference:  canonicalReference,
		platform:            platform,
		source:              chamberImage.SourceBuilt,
		preserveCreatedTime: true,
	})
}

func (s *Store) buildkitBuilder(ctx context.Context) (*buildkit.Builder, error) {
	s.builderMu.Lock()
	defer s.builderMu.Unlock()

	if s.builder != nil {
		return s.builder, nil
	}
	builder, err := buildkit.New(ctx, s.config.BuildKit, s.workspace, s.logger)
	if err != nil {
		return nil, err
	}
	s.builder = &builder
	return s.builder, nil
}

func (s *Store) List(ctx context.Context, request chamberImage.ListRequest) ([]chamberImage.Image, error) {
	if err := s.requireReady(); err != nil {
		return nil, err
	}
	return s.metadata.List(ctx, request)
}

func (s *Store) Remove(ctx context.Context, request chamberImage.RemoveRequest) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.requireReady(); err != nil {
		return err
	}
	canonicalReference, err := chamberImage.CanonicalImageReference(request.Reference)
	if err != nil {
		return err
	}
	platform := chamberImage.NormalizePlatform(request.Platform)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.metadata.Delete(ctx, canonicalReference, platform); err != nil {
		return err
	}
	index, err := readIndex(s.layoutRoot)
	if err != nil {
		return err
	}
	index.Manifests = removeDescriptors(index.Manifests, canonicalReference, platform)
	return writeIndexAtomic(s.workspace, index)
}

func (s *Store) Layout(ctx context.Context) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := s.requireReady(); err != nil {
		return "", err
	}
	return s.layoutRoot, nil
}

func resolvePullRequest(ctx context.Context, request chamberImage.PullRequest) (string, chamberImage.Platform, chamberImage.PullPolicy, error) {
	if ctx == nil {
		return "", chamberImage.Platform{}, "", fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return "", chamberImage.Platform{}, "", fmt.Errorf("%w: image pull canceled before start: %w", chamberErrors.ErrCanceled, err)
	}
	canonicalReference, err := chamberImage.CanonicalImageReference(request.Reference)
	if err != nil {
		return "", chamberImage.Platform{}, "", err
	}
	policy := request.Policy
	switch policy {
	case "", chamberImage.PullIfMissing:
		policy = chamberImage.PullIfMissing
	case chamberImage.PullAlways:
	default:
		return "", chamberImage.Platform{}, "", fmt.Errorf("%w: unsupported pull policy %q", chamberErrors.ErrInvalidRequest, request.Policy)
	}
	return canonicalReference, chamberImage.NormalizePlatform(request.Platform), policy, nil
}

func (s *Store) requireReady() error {
	if s == nil || s.workspace == nil || s.metadata == nil || strings.TrimSpace(s.layoutRoot) == "" || strings.TrimSpace(s.tmpRoot) == "" {
		return fmt.Errorf("%w: image store is not initialized", chamberErrors.ErrInvalidRequest)
	}
	return nil
}

func (s *Store) existingValidImage(ctx context.Context, reference string, platform chamberImage.Platform) (chamberImage.Image, error) {
	existing, err := s.metadata.Get(ctx, reference, platform)
	if err != nil {
		return chamberImage.Image{}, err
	}
	if err := validateStoredImage(ctx, s.layoutRoot, existing, s.workspace); err != nil {
		return chamberImage.Image{}, err
	}
	return existing, nil
}

type commitRequest struct {
	tempLayout          string
	canonicalReference  string
	platform            chamberImage.Platform
	source              chamberImage.Source
	preserveCreatedTime bool
}

func (s *Store) commitLayout(ctx context.Context, request commitRequest) (chamberImage.Image, error) {
	if err := chamberImage.ValidateLayoutContext(ctx, request.tempLayout); err != nil {
		return chamberImage.Image{}, err
	}
	selected, err := selectDescriptor(request.tempLayout, request.canonicalReference, request.platform)
	if err != nil {
		return chamberImage.Image{}, err
	}
	sizeBytes, err := copyReachableBlobs(ctx, request.tempLayout, s.layoutRoot, selected, s.workspace)
	if err != nil {
		return chamberImage.Image{}, err
	}
	index, err := readIndex(s.layoutRoot)
	if err != nil {
		return chamberImage.Image{}, err
	}
	index.Manifests = removeDescriptors(index.Manifests, request.canonicalReference, request.platform)
	index.Manifests = append(index.Manifests, descriptorForCommit(selected, request.canonicalReference, request.platform))
	if err := writeIndexAtomic(s.workspace, index); err != nil {
		return chamberImage.Image{}, err
	}

	committedAt := time.Now().UTC()
	createdAt := committedAt
	if request.preserveCreatedTime {
		existing, err := s.metadata.Get(ctx, request.canonicalReference, request.platform)
		if err == nil && !existing.CreatedAt.IsZero() {
			createdAt = existing.CreatedAt
		} else if err != nil && !errors.Is(err, chamberErrors.ErrImageNotFound) {
			return chamberImage.Image{}, err
		}
	}
	img := chamberImage.Image{
		Reference: request.canonicalReference,
		Digest:    selected.Digest.String(),
		Platform:  request.platform,
		Source:    request.source,
		SizeBytes: sizeBytes,
		CreatedAt: createdAt,
		UpdatedAt: committedAt,
	}
	if err := s.metadata.Put(ctx, img); err != nil {
		return chamberImage.Image{}, err
	}
	chamberLogging.InfoWith(s.logger, ctx, "committed image",
		"image_ref", img.Reference,
		"digest", img.Digest,
		"layout_path", s.layoutRoot,
		"size_bytes", img.SizeBytes,
		"source", string(img.Source),
	)
	return img, nil
}

func initializeStoreLayout(workspace *hostfs.Workspace) error {
	root := filepath.Join(workspace.Root(), "layout")
	if _, err := workspace.MkdirPrivate("layout/blobs"); err != nil {
		return fmt.Errorf("%w: create OCI image layout blobs directory: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	layoutPath := filepath.Join(root, "oci-layout")
	indexPath := filepath.Join(root, "index.json")
	if _, err := os.Stat(layoutPath); errors.Is(err, os.ErrNotExist) {
		file, err := workspace.CreatePrivate("layout/oci-layout")
		if err != nil {
			return fmt.Errorf("%w: initialize OCI image layout metadata: %w", chamberErrors.ErrFilesystemFailed, err)
		}
		if _, err := file.Write([]byte(`{"imageLayoutVersion":"1.0.0"}` + "\n")); err != nil {
			_ = file.Close()
			return fmt.Errorf("%w: initialize OCI image layout metadata: %w", chamberErrors.ErrFilesystemFailed, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("%w: initialize OCI image layout metadata: %w", chamberErrors.ErrFilesystemFailed, err)
		}
	} else if err != nil {
		return fmt.Errorf("%w: inspect OCI image layout metadata: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if _, err := os.Stat(indexPath); errors.Is(err, os.ErrNotExist) {
		if err := writeIndexAtomic(workspace, imagespec.Index{
			Versioned: specs.Versioned{SchemaVersion: 2},
			Manifests: nil,
		}); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("%w: inspect OCI image layout index: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	return validateStoreLayout(root)
}

func validateStoreLayout(root string) error {
	layoutData, err := os.ReadFile(filepath.Join(root, "oci-layout"))
	if err != nil {
		return fmt.Errorf("%w: read OCI image layout metadata: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	var layoutVersion struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if err := json.Unmarshal(layoutData, &layoutVersion); err != nil {
		return fmt.Errorf("%w: decode OCI image layout metadata: %w", chamberErrors.ErrInvalidImageLayout, err)
	}
	if layoutVersion.ImageLayoutVersion == "" {
		return fmt.Errorf("%w: OCI image layout version is missing", chamberErrors.ErrInvalidImageLayout)
	}
	if _, err := readIndex(root); err != nil {
		return err
	}
	blobs, err := os.Stat(filepath.Join(root, "blobs"))
	if err != nil {
		return fmt.Errorf("%w: inspect OCI image layout blobs directory: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if !blobs.IsDir() {
		return fmt.Errorf("%w: OCI image layout blobs path is not a directory", chamberErrors.ErrInvalidImageLayout)
	}
	return nil
}

func selectDescriptor(layoutRoot string, reference string, platform chamberImage.Platform) (imagespec.Descriptor, error) {
	index, err := readIndex(layoutRoot)
	if err != nil {
		return imagespec.Descriptor{}, err
	}
	var exact []imagespec.Descriptor
	var platformOnly []imagespec.Descriptor
	for _, descriptor := range index.Manifests {
		if platformMatches(descriptor.Platform, platform) {
			platformOnly = append(platformOnly, descriptor)
			if descriptor.Annotations[imagespec.AnnotationRefName] == reference {
				exact = append(exact, descriptor)
			}
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0], nil
	case len(exact) > 1:
		return imagespec.Descriptor{}, fmt.Errorf("%w: OCI image layout has multiple manifests for reference %q and platform %s", chamberErrors.ErrInvalidImageLayout, reference, platformString(platform))
	case len(platformOnly) == 1:
		return platformOnly[0], nil
	case len(index.Manifests) == 1:
		return index.Manifests[0], nil
	default:
		return imagespec.Descriptor{}, fmt.Errorf("%w: OCI image layout has no unique manifest for reference %q and platform %s", chamberErrors.ErrInvalidImageLayout, reference, platformString(platform))
	}
}

func descriptorForCommit(descriptor imagespec.Descriptor, reference string, platform chamberImage.Platform) imagespec.Descriptor {
	descriptor.Annotations = cloneAnnotations(descriptor.Annotations)
	descriptor.Annotations[imagespec.AnnotationRefName] = reference
	descriptor.Platform = &imagespec.Platform{
		OS:           platform.OS,
		Architecture: platform.Architecture,
		Variant:      platform.Variant,
	}
	return descriptor
}

func removeDescriptors(descriptors []imagespec.Descriptor, reference string, platform chamberImage.Platform) []imagespec.Descriptor {
	kept := descriptors[:0]
	for _, descriptor := range descriptors {
		if descriptor.Annotations[imagespec.AnnotationRefName] == reference && (descriptor.Platform == nil || platformMatches(descriptor.Platform, platform)) {
			continue
		}
		kept = append(kept, descriptor)
	}
	return kept
}

func readIndex(root string) (imagespec.Index, error) {
	data, err := os.ReadFile(filepath.Join(root, "index.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return imagespec.Index{}, fmt.Errorf("%w: OCI image layout index is missing", chamberErrors.ErrInvalidImageLayout)
		}
		return imagespec.Index{}, fmt.Errorf("%w: read OCI image layout index: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	var index imagespec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return imagespec.Index{}, fmt.Errorf("%w: decode OCI image layout index: %w", chamberErrors.ErrInvalidImageLayout, err)
	}
	if index.SchemaVersion == 0 {
		index.SchemaVersion = 2
	}
	return index, nil
}

func writeIndexAtomic(workspace *hostfs.Workspace, index imagespec.Index) error {
	root := filepath.Join(workspace.Root(), "layout")
	if index.SchemaVersion == 0 {
		index.SchemaVersion = 2
	}
	data, err := json.MarshalIndent(index, "", "\t")
	if err != nil {
		return fmt.Errorf("%w: encode OCI image layout index: %w", chamberErrors.ErrInvalidImageLayout, err)
	}
	data = append(data, '\n')
	tmp, err := workspace.CreateTemp("layout", ".index.json.tmp-*")
	if err != nil {
		return fmt.Errorf("%w: create temporary OCI image layout index: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: write temporary OCI image layout index: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: sync temporary OCI image layout index: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: close temporary OCI image layout index: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := os.Rename(tmpName, filepath.Join(root, "index.json")); err != nil {
		return fmt.Errorf("%w: commit OCI image layout index: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := syncDir(root); err != nil {
		return err
	}
	committed = true
	return nil
}

func copyReachableBlobs(ctx context.Context, src string, dst string, descriptor imagespec.Descriptor, workspace *hostfs.Workspace) (int64, error) {
	seen := make(map[digest.Digest]bool)
	return copyDescriptorGraph(ctx, src, dst, descriptor, seen, workspace)
}

func copyDescriptorGraph(ctx context.Context, src string, dst string, descriptor imagespec.Descriptor, seen map[digest.Digest]bool, workspace *hostfs.Workspace) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("%w: copy OCI image blobs canceled: %w", chamberErrors.ErrCanceled, err)
	}
	if descriptor.Digest == "" {
		return 0, fmt.Errorf("%w: OCI image descriptor is missing digest", chamberErrors.ErrInvalidImageLayout)
	}
	if seen[descriptor.Digest] {
		return 0, nil
	}
	seen[descriptor.Digest] = true
	srcBlob := blobPath(src, descriptor)
	if err := validateBlob(srcBlob, descriptor); err != nil {
		return 0, err
	}
	if err := copyBlobIfMissing(srcBlob, blobPath(dst, descriptor), descriptor, workspace); err != nil {
		return 0, err
	}
	total := descriptor.Size

	switch descriptor.MediaType {
	case imagespec.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		var index imagespec.Index
		if err := decodeBlob(srcBlob, &index); err != nil {
			return 0, err
		}
		for _, child := range index.Manifests {
			size, err := copyDescriptorGraph(ctx, src, dst, child, seen, workspace)
			if err != nil {
				return 0, err
			}
			total += size
		}
	case imagespec.MediaTypeImageManifest, "application/vnd.docker.distribution.manifest.v2+json":
		var manifest imagespec.Manifest
		if err := decodeBlob(srcBlob, &manifest); err != nil {
			return 0, err
		}
		size, err := copyDescriptorGraph(ctx, src, dst, manifest.Config, seen, workspace)
		if err != nil {
			return 0, err
		}
		total += size
		for _, child := range manifest.Layers {
			size, err := copyDescriptorGraph(ctx, src, dst, child, seen, workspace)
			if err != nil {
				return 0, err
			}
			total += size
		}
	}
	return total, nil
}

func validateStoredImage(ctx context.Context, layoutRoot string, img chamberImage.Image, workspace *hostfs.Workspace) error {
	index, err := readIndex(layoutRoot)
	if err != nil {
		return err
	}
	for _, descriptor := range index.Manifests {
		if descriptor.Digest.String() == img.Digest &&
			descriptor.Annotations[imagespec.AnnotationRefName] == img.Reference &&
			platformMatches(descriptor.Platform, img.Platform) {
			_, err := copyReachableBlobs(ctx, layoutRoot, layoutRoot, descriptor, workspace)
			return err
		}
	}
	return fmt.Errorf("%w: shared OCI image layout has no descriptor for image %q digest %s", chamberErrors.ErrInvalidImageLayout, img.Reference, img.Digest)
}

func validateBlob(path string, descriptor imagespec.Descriptor) error {
	if err := descriptor.Digest.Validate(); err != nil {
		return fmt.Errorf("%w: validate OCI image descriptor digest: %w", chamberErrors.ErrInvalidImageLayout, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
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
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: open OCI image layout blob: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	defer file.Close()
	verifier := descriptor.Digest.Verifier()
	if _, err := io.Copy(verifier, file); err != nil {
		return fmt.Errorf("%w: read OCI image layout blob: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if !verifier.Verified() {
		return fmt.Errorf("%w: OCI image layout blob %s content does not match digest", chamberErrors.ErrInvalidImageLayout, descriptor.Digest)
	}
	return nil
}

func copyBlobIfMissing(src string, dst string, descriptor imagespec.Descriptor, workspace *hostfs.Workspace) error {
	if err := validateBlob(dst, descriptor); err == nil {
		return nil
	} else if !errors.Is(err, chamberErrors.ErrInvalidImageLayout) {
		return err
	}

	relParent, err := filepath.Rel(workspace.Root(), filepath.Dir(dst))
	if err != nil {
		return fmt.Errorf("%w: resolve shared OCI image blob parent: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if _, err := workspace.MkdirPrivate(relParent); err != nil {
		return fmt.Errorf("%w: create shared OCI image blob parent: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	tmp, err := workspace.CreateTemp(relParent, "."+descriptor.Digest.Encoded()+".tmp-*")
	if err != nil {
		return fmt.Errorf("%w: create temporary shared OCI image blob: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	srcFile, err := os.Open(src)
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: open temporary OCI image blob: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	_, copyErr := io.Copy(tmp, srcFile)
	closeSrcErr := srcFile.Close()
	if copyErr != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: copy OCI image blob: %w", chamberErrors.ErrFilesystemFailed, copyErr)
	}
	if closeSrcErr != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: close temporary OCI image blob: %w", chamberErrors.ErrFilesystemFailed, closeSrcErr)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: sync shared OCI image blob: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: close shared OCI image blob: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := validateBlob(tmpName, descriptor); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("%w: commit shared OCI image blob: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return err
	}
	committed = true
	return nil
}

func decodeBlob(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: read OCI image layout blob: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("%w: decode OCI image layout blob: %w", chamberErrors.ErrInvalidImageLayout, err)
	}
	return nil
}

func blobPath(root string, descriptor imagespec.Descriptor) string {
	return filepath.Join(root, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())
}

func platformMatches(candidate *imagespec.Platform, requested chamberImage.Platform) bool {
	if candidate == nil {
		return false
	}
	return candidate.OS == requested.OS &&
		candidate.Architecture == requested.Architecture &&
		candidate.Variant == requested.Variant
}

func cloneAnnotations(input map[string]string) map[string]string {
	if input == nil {
		return map[string]string{}
	}
	output := make(map[string]string, len(input)+1)
	for key, value := range input {
		output[key] = value
	}
	return output
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: open directory for sync %q: %w", chamberErrors.ErrFilesystemFailed, path, err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("%w: sync directory %q: %w", chamberErrors.ErrFilesystemFailed, path, err)
	}
	return nil
}

func platformString(platform chamberImage.Platform) string {
	value := platform.OS + "/" + platform.Architecture
	if platform.Variant != "" {
		value += "/" + platform.Variant
	}
	return value
}
