// Package puller provides Chamber's OCI image puller implementation.
// It currently uses go-containerregistry for registry access and OCI layout
// writing.
package puller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"time"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
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
	directoryManager localfs.DirectoryManager
}

func New(directoryManager localfs.DirectoryManager) *Puller {
	return &Puller{
		directoryManager: directoryManager,
	}
}

func (p *Puller) Pull(ctx context.Context, request chamberImage.PullRequest) (chamberImage.PulledImage, error) {
	if p.directoryManager == nil {
		return chamberImage.PulledImage{}, fmt.Errorf("directory manager is required")
	}

	ref, err := name.ParseReference(request.Reference)
	if err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("parse image reference: %w", err)
	}

	platform := resolvePlatform(request.Platform)

	if request.Destination == "" {
		return chamberImage.PulledImage{}, fmt.Errorf("image destination is required")
	}
	destination, err := filepath.Abs(request.Destination)
	if err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("resolve image destination: %w", err)
	}
	parent := filepath.Dir(destination)
	if err := p.directoryManager.EnsurePrivateDir(parent); err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("prepare image destination parent: %w", err)
	}

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
			imagespec.AnnotationRefName: request.Reference,
		}),
	); err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("write OCI image layout: %w", err)
	}
	if err := verifyOCILayout(tmp); err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("verify OCI image layout: %w", err)
	}

	if err := os.Rename(tmp, destination); err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("commit OCI image layout: %w", err)
	}
	renamed = true

	sizeBytes, err := dirSize(destination)
	if err != nil {
		return chamberImage.PulledImage{}, fmt.Errorf("measure OCI image layout: %w", err)
	}

	return chamberImage.PulledImage{
		Reference:  request.Reference,
		Digest:     digest.String(),
		LayoutPath: destination,
		SizeBytes:  sizeBytes,
		PulledAt:   time.Now().UTC(),
	}, nil
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
	layoutPath, err := layout.FromPath(path)
	if err != nil {
		return err
	}
	index, err := layoutPath.ImageIndex()
	if err != nil {
		return err
	}
	_, err = index.IndexManifest()
	return err
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
