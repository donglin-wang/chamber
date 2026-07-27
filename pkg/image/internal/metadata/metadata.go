// Package metadata persists image store records for Chamber's filesystem image
// store.
package metadata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

type Metadata interface {
	Put(ctx context.Context, img chamberImage.Image) error
	Get(ctx context.Context, reference string, platform chamberImage.Platform) (chamberImage.Image, error)
	List(ctx context.Context, request chamberImage.ListRequest) ([]chamberImage.Image, error)
	Delete(ctx context.Context, reference string, platform chamberImage.Platform) error
}

type JSONMetadata struct {
	root       string
	imagesRoot string
	workspace  *hostfs.Workspace
}

var _ Metadata = (*JSONMetadata)(nil)

func NewJSON(root string, workspace *hostfs.Workspace) (*JSONMetadata, error) {
	if workspace == nil {
		return nil, fmt.Errorf("%w: image metadata workspace is required", chamberErrors.ErrInvalidRequest)
	}
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: image metadata root is required", chamberErrors.ErrInvalidRequest)
	}
	imagesRoot := filepath.Join(root, "images")
	rootRel, err := filepath.Rel(workspace.Root(), root)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve image metadata root: %w", chamberErrors.ErrInvalidRequest, err)
	}
	imagesRel, err := filepath.Rel(workspace.Root(), imagesRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve image metadata images root: %w", chamberErrors.ErrInvalidRequest, err)
	}
	if _, err := workspace.MkdirPrivate(rootRel); err != nil {
		return nil, fmt.Errorf("%w: create image metadata root: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if _, err := workspace.MkdirPrivate(imagesRel); err != nil {
		return nil, fmt.Errorf("%w: create image metadata images root: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	return &JSONMetadata{
		root:       root,
		imagesRoot: imagesRoot,
		workspace:  workspace,
	}, nil
}

type metadataFile struct {
	Reference string                `json:"reference"`
	Digest    string                `json:"digest"`
	Platform  chamberImage.Platform `json:"platform"`
	Source    chamberImage.Source   `json:"source"`
	SizeBytes int64                 `json:"size_bytes"`
	CreatedAt string                `json:"created_at"`
	UpdatedAt string                `json:"updated_at"`
}

func (m *JSONMetadata) Put(ctx context.Context, img chamberImage.Image) error {
	if err := requireReady(ctx, m); err != nil {
		return err
	}
	if err := validateImage(img); err != nil {
		return err
	}

	path := m.recordPath(img.Reference, img.Platform)
	imagesRel, err := filepath.Rel(m.workspace.Root(), m.imagesRoot)
	if err != nil {
		return fmt.Errorf("%w: resolve image metadata temp directory: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	file, err := m.workspace.CreateTemp(imagesRel, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("%w: create temporary image metadata file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	tmp := file.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmp)
		}
	}()

	payload := metadataFile{
		Reference: img.Reference,
		Digest:    img.Digest,
		Platform:  chamberImage.NormalizePlatform(img.Platform),
		Source:    img.Source,
		SizeBytes: img.SizeBytes,
		CreatedAt: img.CreatedAt.UTC().Format(timeFormat),
		UpdatedAt: img.UpdatedAt.UTC().Format(timeFormat),
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "\t")
	if err := encoder.Encode(payload); err != nil {
		_ = file.Close()
		return fmt.Errorf("%w: encode image metadata: %w", chamberErrors.ErrMetadataFailed, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("%w: sync image metadata file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("%w: close image metadata file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("%w: commit image metadata file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := syncDir(m.imagesRoot); err != nil {
		return err
	}
	committed = true
	return nil
}

func (m *JSONMetadata) Get(ctx context.Context, reference string, platform chamberImage.Platform) (chamberImage.Image, error) {
	if err := requireReady(ctx, m); err != nil {
		return chamberImage.Image{}, err
	}
	if strings.TrimSpace(reference) == "" {
		return chamberImage.Image{}, fmt.Errorf("%w: image reference is required", chamberErrors.ErrInvalidImageReference)
	}

	data, err := os.ReadFile(m.recordPath(reference, platform))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return chamberImage.Image{}, fmt.Errorf("%w: image metadata record not found", chamberErrors.ErrImageNotFound)
		}
		return chamberImage.Image{}, fmt.Errorf("%w: read image metadata record: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	img, err := decodeImage(data)
	if err != nil {
		return chamberImage.Image{}, err
	}
	return img, nil
}

func (m *JSONMetadata) List(ctx context.Context, request chamberImage.ListRequest) ([]chamberImage.Image, error) {
	if err := requireReady(ctx, m); err != nil {
		return nil, err
	}

	filter := strings.TrimSpace(request.Reference)
	if filter != "" {
		canonical, err := chamberImage.CanonicalImageReference(filter)
		if err != nil {
			return nil, err
		}
		filter = canonical
	}

	entries, err := os.ReadDir(m.imagesRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: list image metadata records: %w", chamberErrors.ErrFilesystemFailed, err)
	}

	images := make([]chamberImage.Image, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.imagesRoot, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("%w: read image metadata record %q: %w", chamberErrors.ErrFilesystemFailed, entry.Name(), err)
		}
		img, err := decodeImage(data)
		if err != nil {
			return nil, err
		}
		if filter == "" || img.Reference == filter {
			images = append(images, img)
		}
	}
	sort.Slice(images, func(i, j int) bool {
		if images[i].Reference != images[j].Reference {
			return images[i].Reference < images[j].Reference
		}
		return platformKey(images[i].Platform) < platformKey(images[j].Platform)
	})
	return images, nil
}

func (m *JSONMetadata) Delete(ctx context.Context, reference string, platform chamberImage.Platform) error {
	if err := requireReady(ctx, m); err != nil {
		return err
	}
	if strings.TrimSpace(reference) == "" {
		return fmt.Errorf("%w: image reference is required", chamberErrors.ErrInvalidImageReference)
	}

	err := os.Remove(m.recordPath(reference, platform))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: delete image metadata record: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err == nil {
		if syncErr := syncDir(m.imagesRoot); syncErr != nil {
			return syncErr
		}
	}
	return nil
}

const timeFormat = "2006-01-02T15:04:05.999999999Z07:00"

func decodeImage(data []byte) (chamberImage.Image, error) {
	var payload metadataFile
	if err := json.Unmarshal(data, &payload); err != nil {
		return chamberImage.Image{}, fmt.Errorf("%w: decode image metadata record: %w", chamberErrors.ErrMetadataFailed, err)
	}
	createdAt, err := parseTime(payload.CreatedAt, "created_at")
	if err != nil {
		return chamberImage.Image{}, err
	}
	updatedAt, err := parseTime(payload.UpdatedAt, "updated_at")
	if err != nil {
		return chamberImage.Image{}, err
	}
	img := chamberImage.Image{
		Reference: payload.Reference,
		Digest:    payload.Digest,
		Platform:  chamberImage.NormalizePlatform(payload.Platform),
		Source:    payload.Source,
		SizeBytes: payload.SizeBytes,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
	if err := validateImage(img); err != nil {
		return chamberImage.Image{}, err
	}
	return img, nil
}

func parseTime(value string, field string) (time.Time, error) {
	parsed, err := time.Parse(timeFormat, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: decode image metadata %s: %w", chamberErrors.ErrMetadataFailed, field, err)
	}
	return parsed.UTC(), nil
}

func validateImage(img chamberImage.Image) error {
	if _, err := chamberImage.CanonicalImageReference(img.Reference); err != nil {
		return err
	}
	if strings.TrimSpace(img.Digest) == "" {
		return fmt.Errorf("%w: image metadata digest is required", chamberErrors.ErrMetadataFailed)
	}
	switch img.Source {
	case chamberImage.SourcePulled, chamberImage.SourceBuilt:
	default:
		return fmt.Errorf("%w: image metadata source is invalid", chamberErrors.ErrMetadataFailed)
	}
	if img.SizeBytes < 0 {
		return fmt.Errorf("%w: image metadata size cannot be negative", chamberErrors.ErrMetadataFailed)
	}
	if img.CreatedAt.IsZero() || img.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: image metadata timestamps are required", chamberErrors.ErrMetadataFailed)
	}
	return nil
}

func requireReady(ctx context.Context, m *JSONMetadata) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || strings.TrimSpace(m.imagesRoot) == "" || m.workspace == nil {
		return fmt.Errorf("%w: image metadata store is not initialized", chamberErrors.ErrInvalidRequest)
	}
	return nil
}

func (m *JSONMetadata) recordPath(reference string, platform chamberImage.Platform) string {
	return filepath.Join(m.imagesRoot, Key(reference, platform)+".json")
}

func Key(reference string, platform chamberImage.Platform) string {
	sum := sha256.Sum256([]byte(reference + "\n" + platformKey(platform)))
	return hex.EncodeToString(sum[:])
}

func platformKey(platform chamberImage.Platform) string {
	normalized := chamberImage.NormalizePlatform(platform)
	return normalized.OS + "/" + normalized.Architecture + "/" + normalized.Variant
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
