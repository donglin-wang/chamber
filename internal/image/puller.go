package image

import (
	"context"
	"time"
)

type PullRequest struct {
	Reference   string
	Destination string
	Platform    string
}

type PulledImage struct {
	Reference  string
	Digest     string
	LayoutPath string
	SizeBytes  int64
	PulledAt   time.Time
}

type Puller interface {
	// Pull writes a complete OCI image layout below Destination. It must write
	// to a temporary sibling first and rename only after verification succeeds.
	Pull(ctx context.Context, request PullRequest) (PulledImage, error)
}
