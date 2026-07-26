// Package registry contains remote registry pull mechanics for Chamber's image
// store.
package registry

import (
	"context"
	"errors"
	"fmt"
	goruntime "runtime"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

func Pull(ctx context.Context, request chamberImage.PullRequest, canonicalReference string, destination string) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: image pull canceled before start: %w", chamberErrors.ErrCanceled, err)
	}

	ref, err := name.ParseReference(canonicalReference)
	if err != nil {
		return fmt.Errorf("%w: parse canonical image reference: %w", chamberErrors.ErrInvalidImageReference, err)
	}
	platform := ResolvePlatform(request.Platform)
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: image pull canceled while fetching image: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return fmt.Errorf("%w: fetch image: %w", chamberErrors.ErrPullFailed, err)
	}
	if _, err := img.Digest(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: image pull canceled while resolving digest: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return fmt.Errorf("%w: resolve image digest: %w", chamberErrors.ErrPullFailed, err)
	}

	layoutPath, err := layout.Write(destination, empty.Index)
	if err != nil {
		return fmt.Errorf("%w: write OCI image layout: %w", chamberErrors.ErrPullFailed, err)
	}
	if err := layoutPath.AppendImage(
		img,
		layout.WithPlatform(platform),
		layout.WithAnnotations(map[string]string{
			imagespec.AnnotationRefName: canonicalReference,
		}),
	); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: image pull canceled while writing image layout: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return fmt.Errorf("%w: write OCI image layout: %w", chamberErrors.ErrPullFailed, err)
	}
	if err := chamberImage.ValidateLayoutContext(ctx, destination); err != nil {
		return generatedLayoutValidationError(err)
	}
	return nil
}

func generatedLayoutValidationError(err error) error {
	if errors.Is(err, chamberErrors.ErrCanceled) {
		return fmt.Errorf("%w: image pull canceled while verifying generated OCI image layout: %w", chamberErrors.ErrCanceled, err)
	}
	return fmt.Errorf("%w: verify generated OCI image layout: %w", chamberErrors.ErrPullFailed, err)
}

func ResolvePlatform(platform chamberImage.Platform) v1.Platform {
	resolved := chamberImage.NormalizePlatform(platform)
	return v1.Platform{
		OS:           resolved.OS,
		Architecture: resolved.Architecture,
		Variant:      resolved.Variant,
	}
}

func HostLinuxPlatform() v1.Platform {
	return v1.Platform{
		OS:           "linux",
		Architecture: goruntime.GOARCH,
	}
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
