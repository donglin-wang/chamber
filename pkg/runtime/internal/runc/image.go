package runc

import (
	"context"
	"errors"
	"fmt"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	"github.com/donglin-wang/chamber/pkg/shared/containerid"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/google/uuid"
)

// RunImage provisions an image in the runtime's bundle workspace and transfers
// ownership of that bundle to the returned container.
func (r *Runtime) RunImage(ctx context.Context, image chamberImage.Image, request chamberRuntime.RunImageRequest) (chamberRuntime.Container, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: image launch canceled before start: %w", chamberErrors.ErrCanceled, err)
	}
	if r == nil || r.provisioner == nil {
		return nil, fmt.Errorf("%w: runtime provisioner is required", chamberErrors.ErrInvalidRequest)
	}
	if request.ContainerID == "" {
		request.ContainerID = "chamber-" + uuid.NewString()
	}
	if err := containerid.Validate(request.ContainerID); err != nil {
		return nil, err
	}
	if request.Process.Terminal == nil {
		terminal := false
		request.Process.Terminal = &terminal
	}
	bundle, err := r.provisioner.Provision(ctx, chamberBundle.ProvisionRequest{
		BundleID:      "image-" + uuid.NewString(),
		ImageLayout:   image.LayoutPath,
		ImageRef:      image.Reference,
		ImageDigest:   image.Digest,
		ImagePlatform: image.Platform,
		Process:       request.Process,
		Mounts:        request.Mounts,
	})
	if err != nil {
		return nil, err
	}
	container, err := r.Run(ctx, chamberRuntime.RunRequest{
		ContainerID: request.ContainerID,
		BundlePath:  bundle.BundlePath,
		Stdin:       request.Stdin,
		Stdout:      request.Stdout,
		Stderr:      request.Stderr,
	})
	if err != nil {
		return nil, errors.Join(err, r.provisioner.Remove(context.Background(), bundle))
	}
	c := container.(*runcContainer)
	c.provisioner = r.provisioner
	c.bundle = bundle
	return c, nil
}
