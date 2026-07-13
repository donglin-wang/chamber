package gocontainerregistry

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	chimage "github.com/donglin-wang/chamber/internal/image"
	"github.com/donglin-wang/chamber/internal/localfs"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

var _ chimage.Puller = (*Puller)(nil)

type Puller struct {
	directoryManager localfs.DirectoryManager
}

func New(directoryManager localfs.DirectoryManager) *Puller {
	return &Puller{
		directoryManager: directoryManager,
	}
}

func (p *Puller) Pull(ctx context.Context, request chimage.PullRequest) (chimage.PulledImage, error) {
	if p.directoryManager == nil {
		return chimage.PulledImage{}, fmt.Errorf("directory manager is required")
	}

	ref, err := name.ParseReference(request.Reference)
	if err != nil {
		return chimage.PulledImage{}, fmt.Errorf("parse image reference: %w", err)
	}

	platform, err := resolvePlatform(request.Platform)
	if err != nil {
		return chimage.PulledImage{}, err
	}

	if request.Destination == "" {
		return chimage.PulledImage{}, fmt.Errorf("image destination is required")
	}
	destination, err := filepath.Abs(request.Destination)
	if err != nil {
		return chimage.PulledImage{}, fmt.Errorf("resolve image destination: %w", err)
	}
	parent := filepath.Dir(destination)
	if err := p.directoryManager.EnsurePrivateDir(parent); err != nil {
		return chimage.PulledImage{}, fmt.Errorf("prepare image destination parent: %w", err)
	}

	img, err := remote.Image(
		ref,
		remote.WithContext(ctx),
		remote.WithPlatform(platform),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	)
	if err != nil {
		return chimage.PulledImage{}, fmt.Errorf("fetch image: %w", err)
	}

	digest, err := img.Digest()
	if err != nil {
		return chimage.PulledImage{}, fmt.Errorf("resolve image digest: %w", err)
	}

	tmp, err := p.directoryManager.MkdirTemp(parent, "."+filepath.Base(destination)+".tmp-*")
	if err != nil {
		return chimage.PulledImage{}, fmt.Errorf("create temporary image layout: %w", err)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.RemoveAll(tmp)
		}
	}()

	layoutPath, err := layout.Write(tmp, empty.Index)
	if err != nil {
		return chimage.PulledImage{}, fmt.Errorf("write OCI image layout: %w", err)
	}
	if err := layoutPath.AppendImage(img, layout.WithPlatform(platform)); err != nil {
		return chimage.PulledImage{}, fmt.Errorf("write OCI image layout: %w", err)
	}
	if err := verifyOCILayout(tmp); err != nil {
		return chimage.PulledImage{}, fmt.Errorf("verify OCI image layout: %w", err)
	}

	if err := os.Rename(tmp, destination); err != nil {
		return chimage.PulledImage{}, fmt.Errorf("commit OCI image layout: %w", err)
	}
	renamed = true

	sizeBytes, err := dirSize(destination)
	if err != nil {
		return chimage.PulledImage{}, fmt.Errorf("measure OCI image layout: %w", err)
	}

	return chimage.PulledImage{
		Reference:  ref.Name(),
		Digest:     digest.String(),
		LayoutPath: destination,
		SizeBytes:  sizeBytes,
		PulledAt:   time.Now().UTC(),
	}, nil
}

func resolvePlatform(raw string) (v1.Platform, error) {
	if raw == "" {
		return v1.Platform{
			OS:           "linux",
			Architecture: runtime.GOARCH,
		}, nil
	}

	platform, err := v1.ParsePlatform(raw)
	if err != nil {
		return v1.Platform{}, fmt.Errorf("parse image platform: %w", err)
	}
	if platform.OS != "linux" || platform.Architecture != runtime.GOARCH || platform.OSVersion != "" || platform.Variant != "" {
		return v1.Platform{}, fmt.Errorf("unsupported image platform %q: only linux/%s is supported", raw, runtime.GOARCH)
	}

	return *platform, nil
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
