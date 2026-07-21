package image

import (
	"fmt"

	chamberImagePuller "github.com/donglin-wang/chamber/pkg/image/puller"
	chamberImageShared "github.com/donglin-wang/chamber/pkg/image/shared"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

func NewPuller(config chamberImageShared.Config, directoryManager localfs.DirectoryManager) (chamberImageShared.Puller, error) {
	if directoryManager == nil {
		return nil, fmt.Errorf("%w: directory manager is required", chamberErrors.ErrInvalidRequest)
	}
	if config.Root == "" {
		return nil, chamberImageShared.ErrRootRequired
	}
	if err := directoryManager.MkdirPrivate(config.Root); err != nil {
		return nil, fmt.Errorf("%w: create image root: %v", chamberErrors.ErrFilesystemFailed, err)
	}
	return chamberImagePuller.New(config, directoryManager)
}
