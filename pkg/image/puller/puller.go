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

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
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

var _ chamberImage.Puller = (*Puller)(nil)

type Puller struct {
	config           chamberImage.Config
	directoryManager localfs.DirectoryManager
	logger           *chamberLogging.SlogLogger
}

func New(config chamberImage.Config, directoryManager localfs.DirectoryManager) (*Puller, error) {
	if directoryManager == nil {
		return nil, fmt.Errorf("directory manager is required")
	}
	resolved, err := chamberImage.Resolve(config, chamberImage.Override{})
	if err != nil {
		return nil, err
	}
	logger, err := chamberLogging.ResolveLogger(resolved.Logging, nil)
	if err != nil {
		return nil, err
	}
	if resolved.Root == "" {
		return nil, chamberImage.ErrRootRequired
	}
	if err := directoryManager.MkdirPrivate(resolved.Root); err != nil {
		return nil, fmt.Errorf("create image root: %w", err)
	}

	return &Puller{
		config:           resolved,
		directoryManager: directoryManager,
		logger:           logger,
	}, nil
}

func (p *Puller) Pull(ctx context.Context, request chamberImage.PullRequest) (chamberImage.PulledImage, error) {
	if p == nil || p.directoryManager == nil {
		return chamberImage.PulledImage{}, fmt.Errorf("directory manager is required")
	}

	ref, err := name.ParseReference(request.Reference)
	if err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("parse image reference: %w", err)
	}
	canonicalReference := canonicalReferenceName(ref)

	platform := resolvePlatform(request.Platform)

	destination, err := chamberImage.Destination(p.config.Root, canonicalReference)
	if err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("resolve image destination: %w", err)
	}
	parent := filepath.Dir(destination)
	if err := p.directoryManager.MkdirPrivate(parent); err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("create image destination parent: %w", err)
	}
	if chamberImage.LayoutExists(destination) {
		pulled, err := existingPulledImage(canonicalReference, destination)
		if err != nil {
			return chamberImage.PulledImage{}, err
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
		return chamberImage.PulledImage{}, fmt.Errorf("fetch image: %w", err)
	}

	digest, err := img.Digest()
	if err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("resolve image digest: %w", err)
	}

	tmp, err := p.directoryManager.MkdirTemp(parent, "."+filepath.Base(destination)+".tmp-*")
	if err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("create temporary image layout: %w", err)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.RemoveAll(tmp)
		}
	}()

	layoutPath, err := layout.Write(tmp, empty.Index)
	if err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("write OCI image layout: %w", err)
	}
	if err := layoutPath.AppendImage(
		img,
		layout.WithPlatform(platform),
		layout.WithAnnotations(map[string]string{
			imagespec.AnnotationRefName: canonicalReference,
		}),
	); err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("write OCI image layout: %w", err)
	}
	if err := verifyOCILayout(tmp); err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("verify OCI image layout: %w", err)
	}

	if err := os.Rename(tmp, destination); err != nil {
		if chamberImage.LayoutExists(destination) {
			pulled, existingErr := existingPulledImage(canonicalReference, destination)
			if existingErr != nil {
				return chamberImage.PulledImage{}, existingErr
			}
			return pulled, nil
		}
		return chamberImage.PulledImage{}, fmt.Errorf("commit OCI image layout: %w", err)
	}
	renamed = true

	sizeBytes, err := dirSize(destination)
	if err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("measure OCI image layout: %w", err)
	}

	pulled := chamberImage.PulledImage{
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

func resolvePlatform(platform chamberImage.Platform) v1.Platform {
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

func canonicalReferenceName(ref name.Reference) string {
	return ref.Name()
}

func authenticator(auth *chamberImage.Auth) authn.Authenticator {
	config := authn.AuthConfig{
		Username: auth.Username,
		Password: auth.Password,
	}
	if auth.Token != "" {
		config.RegistryToken = auth.Token
	}
	return authn.FromConfig(config)
}

func verifyOCILayout(path string) error {
	return chamberImage.ValidateLayout(path)
}

func existingPulledImage(reference string, path string) (chamberImage.PulledImage, error) {
	layoutPath, err := layout.FromPath(path)
	if err != nil {
		return chamberImage.PulledImage{}, err
	}
	index, err := layoutPath.ImageIndex()
	if err != nil {
		return chamberImage.PulledImage{}, err
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return chamberImage.PulledImage{}, err
	}
	if len(manifest.Manifests) == 0 {
		return chamberImage.PulledImage{}, errors.New("OCI image layout index has no manifests")
	}
	descriptor := manifest.Manifests[0]
	for _, candidate := range manifest.Manifests {
		if candidate.Annotations[imagespec.AnnotationRefName] == reference {
			descriptor = candidate
			break
		}
	}
	sizeBytes, err := dirSize(path)
	if err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("measure OCI image layout: %w", err)
	}
	return chamberImage.PulledImage{
		Reference:  reference,
		Digest:     descriptor.Digest.String(),
		LayoutPath: path,
		SizeBytes:  sizeBytes,
		PulledAt:   time.Now().UTC(),
	}, nil
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
