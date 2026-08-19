package factory

import (
	"context"
	"fmt"
	"strings"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageStore "github.com/donglin-wang/chamber/pkg/image/internal/store"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	"github.com/donglin-wang/chamber/pkg/shared/hostprobe"
)

var imageStoreHostRules = []hostprobe.Rule{
	hostprobe.RequireLinux,
}

var checkImageStoreHostRules = requireImageStoreHostRules

var imageStoreWorkspaceRequirements = hostfs.FeatureSet{
	PrivateDirs:           true,
	FileFsync:             true,
	AtomicFileRename:      true,
	AtomicDirectoryRename: true,
}

// NewStore validates config, creates the configured private image root and
// package workspace, and returns a ready image store. Callers remain
// responsible for root placement, process-level coordination, cleanup policy,
// cancellation policy, and recovery.
func NewStore(config chamberImage.Config) (chamberImage.Store, error) {
	if config.Root == "" {
		return nil, chamberImage.ErrRootRequired
	}
	workspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:         config.Root,
		TmpRoot:      config.TmpRoot,
		Requirements: imageStoreWorkspaceRequirements,
	})
	if err != nil {
		return nil, err
	}
	return NewStoreWithWorkspace(config, workspace)
}

// NewStoreWithWorkspace validates config and the supplied package workspace,
// creates the configured private image root and
// store-owned directories, and returns a ready image store. Callers remain
// responsible for root placement, process-level coordination, cleanup policy,
// cancellation policy, and recovery.
func NewStoreWithWorkspace(config chamberImage.Config, workspace *hostfs.Workspace) (chamberImage.Store, error) {
	if workspace == nil {
		return nil, fmt.Errorf("%w: image workspace is required", chamberErrors.ErrInvalidRequest)
	}
	if config.Root == "" {
		return nil, chamberImage.ErrRootRequired
	}
	if err := checkImageStoreHostRules(context.Background()); err != nil {
		return nil, err
	}
	return chamberImageStore.New(config, workspace)
}

func requireImageStoreHostRules(ctx context.Context) error {
	var messages []string
	for _, rule := range imageStoreHostRules {
		messages = append(messages, rule.Check(ctx)...)
	}
	if len(messages) == 0 {
		return nil
	}
	return fmt.Errorf("%w: image store host probe failed: %s", chamberErrors.ErrUnsupportedHost, strings.Join(messages, "; "))
}
