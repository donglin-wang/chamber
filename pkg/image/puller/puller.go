// Package puller provides Chamber's OCI image puller implementation.
// It currently uses go-containerregistry for registry access and OCI layout
// writing.
package puller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"time"

	chamberImageShared "github.com/donglin-wang/chamber/pkg/image/shared"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/imageref"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

var _ chamberImageShared.Puller = (*Puller)(nil)

type Puller struct {
	config           chamberImageShared.Config
	directoryManager localfs.DirectoryManager
	logger           *chamberLogging.SlogLogger
}

func New(config chamberImageShared.Config, directoryManager localfs.DirectoryManager) (*Puller, error) {
	logger, err := chamberLogging.LoggerFromConfig(config.Logging, nil)
	if err != nil {
		return nil, err
	}

	return &Puller{
		config:           config,
		directoryManager: directoryManager,
		logger:           logger,
	}, nil
}

func (p *Puller) Pull(ctx context.Context, request chamberImageShared.PullRequest) (chamberImageShared.PulledImage, error) {
	if p == nil || p.directoryManager == nil {
		return chamberImageShared.PulledImage{}, fmt.Errorf("%w: directory manager is required", chamberErrors.ErrInvalidRequest)
	}

	canonicalReference, err := imageref.Canonical(request.Reference)
	if err != nil {
		return chamberImageShared.PulledImage{}, err
	}
	ref, err := name.ParseReference(canonicalReference)
	if err != nil {
		return chamberImageShared.PulledImage{}, fmt.Errorf("%w: parse canonical image reference: %w", chamberErrors.ErrInvalidRequest, err)
	}

	platform := resolvePlatform(request.Platform)
	policy := request.Policy
	switch policy {
	case "", chamberImageShared.PullIfMissing:
		policy = chamberImageShared.PullIfMissing
	case chamberImageShared.PullAlways:
	default:
		return chamberImageShared.PulledImage{}, fmt.Errorf("%w: unsupported pull policy %q", chamberErrors.ErrInvalidRequest, request.Policy)
	}

	destination, err := chamberImageShared.DestinationForCanonicalImage(p.config.Root, canonicalReference, request.Platform)
	if err != nil {
		return chamberImageShared.PulledImage{}, fmt.Errorf("resolve image destination: %w", err)
	}
	parent := filepath.Dir(destination)
	if err := p.directoryManager.MkdirPrivate(parent); err != nil {
		return chamberImageShared.PulledImage{}, fmt.Errorf("create image destination parent: %w", err)
	}
	if policy == chamberImageShared.PullIfMissing && chamberImageShared.LayoutExists(destination) {
		pulled, err := existingPulledImage(canonicalReference, platform, destination)
		if err != nil {
			return chamberImageShared.PulledImage{}, err
		}
		chamberLogging.InfoWith(p.logger, ctx, "reused image layout",
			"image_ref", pulled.Reference,
			"layout_path", pulled.LayoutPath,
			"digest", pulled.Digest,
			"size_bytes", pulled.SizeBytes,
		)
		return pulled, nil
	}

	chamberLogging.InfoWith(p.logger, ctx, "pulling image",
		"image_ref", canonicalReference,
		"destination", destination,
		"platform_os", platform.OS,
		"platform_architecture", platform.Architecture,
		"platform_variant", platform.Variant,
	)

	options := []remote.Option{
		remote.WithContext(ctx),
		remote.WithPlatform(platform),
	}
	if request.Auth == nil {
		options = append(options, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	} else {
		options = append(options, remote.WithAuth(authenticator(request.Auth)))
	}

	img, err := remote.Image(ref, options...)
	if err != nil {
		return chamberImageShared.PulledImage{}, fmt.Errorf("fetch image: %w", err)
	}

	digest, err := img.Digest()
	if err != nil {
		return chamberImageShared.PulledImage{}, fmt.Errorf("resolve image digest: %w", err)
	}

	tmp, err := p.directoryManager.MkdirTemp(parent, "."+filepath.Base(destination)+".tmp-*")
	if err != nil {
		return chamberImageShared.PulledImage{}, fmt.Errorf("create temporary image layout: %w", err)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.RemoveAll(tmp)
		}
	}()

	layoutPath, err := layout.Write(tmp, empty.Index)
	if err != nil {
		return chamberImageShared.PulledImage{}, fmt.Errorf("write OCI image layout: %w", err)
	}
	if err := layoutPath.AppendImage(
		img,
		layout.WithPlatform(platform),
		layout.WithAnnotations(map[string]string{
			imagespec.AnnotationRefName: canonicalReference,
		}),
	); err != nil {
		return chamberImageShared.PulledImage{}, fmt.Errorf("write OCI image layout: %w", err)
	}
	if err := chamberImageShared.ValidateLayout(tmp); err != nil {
		return chamberImageShared.PulledImage{}, fmt.Errorf("verify OCI image layout: %w", err)
	}

	backup := ""
	if policy == chamberImageShared.PullAlways {
		existing, err := moveExistingLayout(parent, filepath.Base(destination), destination)
		if err != nil {
			return chamberImageShared.PulledImage{}, err
		}
		backup = existing
	}
	if err := os.Rename(tmp, destination); err != nil {
		if backup != "" {
			if restoreErr := os.Rename(backup, destination); restoreErr != nil {
				return chamberImageShared.PulledImage{}, fmt.Errorf("commit OCI image layout: %w; restore previous layout: %v", err, restoreErr)
			}
		}
		if policy == chamberImageShared.PullIfMissing && chamberImageShared.LayoutExists(destination) {
			pulled, existingErr := existingPulledImage(canonicalReference, platform, destination)
			if existingErr != nil {
				return chamberImageShared.PulledImage{}, existingErr
			}
			return pulled, nil
		}
		return chamberImageShared.PulledImage{}, fmt.Errorf("commit OCI image layout: %w", err)
	}
	renamed = true
	if backup != "" {
		_ = os.RemoveAll(backup)
	}

	sizeBytes, err := dirSize(destination)
	if err != nil {
		return chamberImageShared.PulledImage{}, fmt.Errorf("measure OCI image layout: %w", err)
	}

	pulled := chamberImageShared.PulledImage{
		Reference:  canonicalReference,
		Digest:     digest.String(),
		LayoutPath: destination,
		SizeBytes:  sizeBytes,
		PulledAt:   time.Now().UTC(),
	}
	chamberLogging.InfoWith(p.logger, ctx, "pulled image",
		"image_ref", pulled.Reference,
		"digest", pulled.Digest,
		"layout_path", pulled.LayoutPath,
		"size_bytes", pulled.SizeBytes,
	)
	return pulled, nil
}

func resolvePlatform(platform chamberImageShared.Platform) v1.Platform {
	resolved := v1.Platform{
		OS:           "linux",
		Architecture: goruntime.GOARCH,
	}
	if platform.OS != "" {
		resolved.OS = platform.OS
	}
	if platform.Architecture != "" {
		resolved.Architecture = platform.Architecture
	}
	if platform.Variant != "" {
		resolved.Variant = platform.Variant
	}
	return resolved
}

func authenticator(auth *chamberImageShared.Auth) authn.Authenticator {
	config := authn.AuthConfig{
		Username: auth.Username,
		Password: auth.Password,
	}
	if auth.Token != "" {
		config.RegistryToken = auth.Token
	}
	return authn.FromConfig(config)
}

func moveExistingLayout(parent string, base string, destination string) (string, error) {
	if _, err := os.Stat(destination); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("inspect existing OCI image layout: %w", err)
	}
	backup, err := os.MkdirTemp(parent, "."+base+".old-*")
	if err != nil {
		return "", fmt.Errorf("create previous OCI image layout backup: %w", err)
	}
	if err := os.Remove(backup); err != nil {
		return "", fmt.Errorf("prepare previous OCI image layout backup: %w", err)
	}
	if err := os.Rename(destination, backup); err != nil {
		return "", fmt.Errorf("move previous OCI image layout aside: %w", err)
	}
	return backup, nil
}

func existingPulledImage(reference string, platform v1.Platform, path string) (chamberImageShared.PulledImage, error) {
	layoutPath, err := layout.FromPath(path)
	if err != nil {
		return chamberImageShared.PulledImage{}, err
	}
	index, err := layoutPath.ImageIndex()
	if err != nil {
		return chamberImageShared.PulledImage{}, err
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return chamberImageShared.PulledImage{}, err
	}
	if len(manifest.Manifests) == 0 {
		return chamberImageShared.PulledImage{}, errors.New("OCI image layout index has no manifests")
	}
	var descriptor v1.Descriptor
	found := false
	for _, candidate := range manifest.Manifests {
		if candidate.Annotations[imagespec.AnnotationRefName] == reference && platformMatches(candidate.Platform, platform) {
			descriptor = candidate
			found = true
			break
		}
	}
	if !found {
		return chamberImageShared.PulledImage{}, fmt.Errorf("OCI image layout has no manifest for reference %q and platform %s/%s", reference, platform.OS, platform.Architecture)
	}
	sizeBytes, err := dirSize(path)
	if err != nil {
		return chamberImageShared.PulledImage{}, fmt.Errorf("measure OCI image layout: %w", err)
	}
	return chamberImageShared.PulledImage{
		Reference:  reference,
		Digest:     descriptor.Digest.String(),
		LayoutPath: path,
		SizeBytes:  sizeBytes,
		PulledAt:   time.Now().UTC(),
	}, nil
}

func platformMatches(candidate *v1.Platform, requested v1.Platform) bool {
	if candidate == nil {
		return false
	}
	return candidate.OS == requested.OS &&
		candidate.Architecture == requested.Architecture &&
		candidate.Variant == requested.Variant
}

func dirSize(path string) (int64, error) {
	var size int64
	err := filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	return size, err
}
