package factory

import (
	"fmt"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageStore "github.com/donglin-wang/chamber/pkg/image/internal/store"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

// NewStore validates config, creates the configured private image root and
// store-owned directories, and returns a ready image store. Callers remain
// responsible for root placement, process-level coordination, cleanup policy,
// cancellation policy, and recovery.
func NewStore(config chamberImage.Config, directoryManager localfs.DirectoryManager) (chamberImage.Store, error) {
	if directoryManager == nil {
		return nil, fmt.Errorf("%w: directory manager is required", chamberErrors.ErrInvalidRequest)
	}
	if config.Root == "" {
		return nil, chamberImage.ErrRootRequired
	}
	return chamberImageStore.New(config, directoryManager)
}
