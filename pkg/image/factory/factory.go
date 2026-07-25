package factory

import (
	"fmt"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImagePuller "github.com/donglin-wang/chamber/pkg/image/internal/puller"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

// NewPuller validates config, creates the configured private image root, and
// returns a ready image puller. The returned puller stores OCI image layouts
// below config.Root; callers remain responsible for root placement, locking,
// cleanup, cancellation policy, and recovery.
func NewPuller(config chamberImage.Config, directoryManager localfs.DirectoryManager) (chamberImage.Puller, error) {
	if directoryManager == nil {
		return nil, fmt.Errorf("%w: directory manager is required", chamberErrors.ErrInvalidRequest)
	}
	if config.Root == "" {
		return nil, chamberImage.ErrRootRequired
	}
	if err := directoryManager.MkdirPrivate(config.Root); err != nil {
		return nil, fmt.Errorf("%w: create image root: %v", chamberErrors.ErrFilesystemFailed, err)
	}
	return chamberImagePuller.New(config, directoryManager)
}
