package factory

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberDirectoryProvisioner "github.com/donglin-wang/chamber/pkg/bundle/internal/directory"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	"github.com/donglin-wang/chamber/pkg/shared/hostprobe"
)

var provisionerNames = map[string]struct{}{
	chamberBundle.ProvisionerNameDirectory: {},
}

var provisionerHostRules = []hostprobe.Rule{
	hostprobe.RequireLinux,
	hostprobe.RequireRootlessUser,
}

var checkProvisionerHostRules = requireProvisionerHostRules

var provisionerWorkspaceRequirements = hostfs.FeatureSet{
	PrivateDirs:           true,
	AtomicDirectoryRename: true,
}

// NewProvisioner validates config, creates the configured private bundle root,
// creates the package workspace, checks the selected implementation name, and
// returns a ready bundle provisioner. Callers own bundle-root placement,
// cleanup, cancellation policy, and recovery.
func NewProvisioner(config chamberBundle.Config) (chamberBundle.Provisioner, error) {
	workspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:         config.Root,
		TmpRoot:      config.TmpRoot,
		Requirements: provisionerWorkspaceRequirements,
	})
	if err != nil {
		return nil, err
	}
	return NewProvisionerWithWorkspace(config, workspace)
}

// NewProvisionerWithWorkspace validates config and the supplied package
// workspace, creates the configured private bundle root,
// checks the selected implementation name, and returns a ready bundle
// provisioner. Callers own bundle-root placement, cleanup, cancellation policy,
// and recovery.
func NewProvisionerWithWorkspace(config chamberBundle.Config, workspace *hostfs.Workspace) (chamberBundle.Provisioner, error) {
	if workspace == nil {
		return nil, fmt.Errorf("%w: bundle workspace is required", chamberErrors.ErrInvalidRequest)
	}
	if config.Name == "" {
		return nil, fmt.Errorf("%w: bundle provisioner name is required", chamberErrors.ErrInvalidRequest)
	}
	if !IsSupportedProvisionerName(config.Name) {
		return nil, fmt.Errorf("%w: unsupported bundle provisioner name %q (supported: %s)", chamberErrors.ErrInvalidRequest, config.Name, strings.Join(SupportedProvisionerNames(), ", "))
	}
	if config.Root == "" {
		return nil, fmt.Errorf("%w: bundle root is required", chamberErrors.ErrInvalidRequest)
	}
	configRoot, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve bundle root: %w", chamberErrors.ErrInvalidRequest, err)
	}
	if filepath.Clean(configRoot) != filepath.Clean(workspace.Root()) {
		return nil, fmt.Errorf("%w: bundle root %q does not match workspace root %q", chamberErrors.ErrInvalidRequest, config.Root, workspace.Root())
	}
	if err := requireWorkspaceTmpRoot("bundle temporary root", config.TmpRoot, workspace.TmpRoot()); err != nil {
		return nil, err
	}
	if err := requireWorkspaceFeatures("bundle workspace", workspace.Features(), hostfs.FeatureSet{
		PrivateDirs:           provisionerWorkspaceRequirements.PrivateDirs,
		AtomicDirectoryRename: provisionerWorkspaceRequirements.AtomicDirectoryRename,
	}); err != nil {
		return nil, err
	}
	if err := checkProvisionerHostRules(context.Background()); err != nil {
		return nil, err
	}
	config.Root = workspace.Root()
	config.TmpRoot = workspace.TmpRoot()

	switch config.Name {
	case chamberBundle.ProvisionerNameDirectory:
		return chamberDirectoryProvisioner.New(config, workspace)
	default:
		return nil, fmt.Errorf("%w: unsupported bundle provisioner name %q (supported: %s)", chamberErrors.ErrInvalidRequest, config.Name, strings.Join(SupportedProvisionerNames(), ", "))
	}
}

func requireProvisionerHostRules(ctx context.Context) error {
	var messages []string
	for _, rule := range provisionerHostRules {
		messages = append(messages, rule.Check(ctx)...)
	}
	if len(messages) == 0 {
		return nil
	}
	return fmt.Errorf("%w: bundle provisioner host probe failed: %s", chamberErrors.ErrUnsupportedHost, strings.Join(messages, "; "))
}

// SupportedProvisionerNames returns the sorted list of provisioner
// implementation names accepted by NewProvisioner.
func SupportedProvisionerNames() []string {
	names := make([]string, 0, len(provisionerNames))
	for name := range provisionerNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsSupportedProvisionerName reports whether name selects a provisioner
// implementation known to this package.
func IsSupportedProvisionerName(name string) bool {
	_, ok := provisionerNames[name]
	return ok
}

func requireWorkspaceTmpRoot(label string, configured string, actual string) error {
	if strings.TrimSpace(configured) == "" {
		return nil
	}
	configuredAbs, err := filepath.Abs(configured)
	if err != nil {
		return fmt.Errorf("%w: resolve %s %q: %w", chamberErrors.ErrInvalidRequest, label, configured, err)
	}
	if filepath.Clean(configuredAbs) != filepath.Clean(actual) {
		return fmt.Errorf("%w: %s %q does not match workspace temporary root %q", chamberErrors.ErrInvalidRequest, label, configured, actual)
	}
	return nil
}

func requireWorkspaceFeatures(label string, observed hostfs.FeatureSet, required hostfs.FeatureSet) error {
	if required.PrivateDirs && !observed.PrivateDirs {
		return fmt.Errorf("%w: %s requires private directories", chamberErrors.ErrFilesystemFailed, label)
	}
	if required.FileFsync && !observed.FileFsync {
		return fmt.Errorf("%w: %s requires file fsync", chamberErrors.ErrFilesystemFailed, label)
	}
	if required.DirectoryFsync && !observed.DirectoryFsync {
		return fmt.Errorf("%w: %s requires directory fsync", chamberErrors.ErrFilesystemFailed, label)
	}
	if required.AtomicFileRename && !observed.AtomicFileRename {
		return fmt.Errorf("%w: %s requires atomic file rename between temporary and durable roots", chamberErrors.ErrFilesystemFailed, label)
	}
	if required.AtomicDirectoryRename && !observed.AtomicDirectoryRename {
		return fmt.Errorf("%w: %s requires atomic directory rename between temporary and durable roots", chamberErrors.ErrFilesystemFailed, label)
	}
	return nil
}
