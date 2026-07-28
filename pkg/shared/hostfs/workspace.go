package hostfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	securejoin "github.com/cyphar/filepath-securejoin"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

type Config struct {
	Root         string
	TmpRoot      string
	Capabilities Capabilities
}

type Capabilities struct {
	PrivateDirs           bool
	FileFsync             bool
	DirectoryFsync        bool
	AtomicFileRename      bool
	AtomicDirectoryRename bool
}

type Workspace struct {
	root    string
	tmpRoot string
	caps    Capabilities
	ops     filesystemOps
}

var errDirectorySyncUnsupported = errors.New("directory sync unsupported")

type filesystemOps struct {
	mkdirAll  func(string, fs.FileMode) error
	stat      func(string) (os.FileInfo, error)
	openFile  func(string, int, fs.FileMode) (*os.File, error)
	rename    func(string, string) error
	remove    func(string) error
	removeAll func(string) error
	syncDir   func(string) error
}

func defaultFilesystemOps() filesystemOps {
	return filesystemOps{
		mkdirAll:  os.MkdirAll,
		stat:      os.Stat,
		openFile:  os.OpenFile,
		rename:    os.Rename,
		remove:    os.Remove,
		removeAll: os.RemoveAll,
		syncDir:   syncDirectory,
	}
}

func NewWorkspace(config Config) (*Workspace, error) {
	return newWorkspace(config, defaultFilesystemOps())
}

// DefaultTmpRoot returns Chamber's user-scoped temporary root below the host
// temporary directory. Name, when set, selects a package-specific subdirectory.
func DefaultTmpRoot(name string) string {
	root := filepath.Join(os.TempDir(), fmt.Sprintf("chamber-%d", os.Geteuid()))
	name = strings.TrimSpace(name)
	if name == "" {
		return root
	}
	return filepath.Join(root, name)
}

func newWorkspace(config Config, ops filesystemOps) (*Workspace, error) {
	ops = fillFilesystemOps(ops)
	root, err := absolutePath("workspace root", config.Root)
	if err != nil {
		return nil, err
	}
	tmpRoot := strings.TrimSpace(config.TmpRoot)
	if tmpRoot == "" {
		tmpRoot = DefaultTmpRoot("")
	}
	tmpRoot, err = absolutePath("workspace temporary root", tmpRoot)
	if err != nil {
		return nil, err
	}

	workspace := &Workspace{
		root:    root,
		tmpRoot: tmpRoot,
		ops:     ops,
	}
	rootPrivate, err := workspace.checkWorkspaceDir(root, "workspace root", config.Capabilities.PrivateDirs)
	if err != nil {
		return nil, err
	}
	tmpPrivate, err := workspace.checkWorkspaceDir(tmpRoot, "workspace temporary root", config.Capabilities.PrivateDirs)
	if err != nil {
		return nil, err
	}
	caps, err := workspace.probeCapabilities(rootPrivate && tmpPrivate)
	if err != nil {
		return nil, err
	}
	workspace.caps = caps
	if err := requireCapabilities(config.Capabilities, caps); err != nil {
		return nil, err
	}
	return workspace, nil
}

func (w *Workspace) Root() string {
	if w == nil {
		return ""
	}
	return w.root
}

func (w *Workspace) TmpRoot() string {
	if w == nil {
		return ""
	}
	return w.tmpRoot
}

func (w *Workspace) Capabilities() Capabilities {
	if w == nil {
		return Capabilities{}
	}
	return w.caps
}

func (w *Workspace) MkdirPrivate(relDir string) (string, error) {
	if err := w.requireReady(); err != nil {
		return "", err
	}
	path, err := w.rootPath(relDir)
	if err != nil {
		return "", err
	}
	if err := w.mkdirPrivateAbs(path, "private directory"); err != nil {
		return "", err
	}
	return path, nil
}

func (w *Workspace) CreatePrivate(rel string) (*os.File, error) {
	if err := w.requireReady(); err != nil {
		return nil, err
	}
	path, err := w.rootPath(rel)
	if err != nil {
		return nil, err
	}
	if filepath.Base(path) == "." {
		return nil, fmt.Errorf("%w: private file path is required", chamberErrors.ErrInvalidRequest)
	}
	parentRel, err := filepath.Rel(w.root, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%w: resolve private file parent %q: %w", chamberErrors.ErrInvalidRequest, path, err)
	}
	if _, err := w.MkdirPrivate(parentRel); err != nil {
		return nil, err
	}
	file, err := w.ops.openFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: create private file %q: already exists", chamberErrors.ErrInvalidRequest, path)
		}
		return nil, fmt.Errorf("%w: create private file %q: %w", chamberErrors.ErrFilesystemFailed, path, err)
	}
	return file, nil
}

func (w *Workspace) MkdirTemp(relDir string, pattern string) (string, error) {
	if err := w.requireReady(); err != nil {
		return "", err
	}
	parent, err := w.tmpPath(relDir)
	if err != nil {
		return "", err
	}
	if err := w.mkdirPrivateAbs(parent, "temporary directory parent"); err != nil {
		return "", err
	}
	path, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return "", fmt.Errorf("%w: create temporary directory below %q: %w", chamberErrors.ErrFilesystemFailed, parent, err)
	}
	return path, nil
}

func (w *Workspace) CreateTemp(relDir string, pattern string) (*os.File, error) {
	if err := w.requireReady(); err != nil {
		return nil, err
	}
	parent, err := w.tmpPath(relDir)
	if err != nil {
		return nil, err
	}
	if err := w.mkdirPrivateAbs(parent, "temporary file parent"); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(parent, pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: create temporary file below %q: %w", chamberErrors.ErrFilesystemFailed, parent, err)
	}
	return file, nil
}

func (w *Workspace) requireReady() error {
	if w == nil || strings.TrimSpace(w.root) == "" || strings.TrimSpace(w.tmpRoot) == "" {
		return fmt.Errorf("%w: host filesystem workspace is not initialized", chamberErrors.ErrInvalidRequest)
	}
	return nil
}

func (w *Workspace) rootPath(rel string) (string, error) {
	return scopedPath(w.root, rel)
}

func (w *Workspace) tmpPath(rel string) (string, error) {
	return scopedPath(w.tmpRoot, rel)
}

func scopedPath(root string, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("%w: workspace-relative path is required", chamberErrors.ErrInvalidRequest)
	}
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: workspace-relative path %q escapes workspace", chamberErrors.ErrInvalidRequest, rel)
	}
	if err := rejectSymlinkComponents(root, clean); err != nil {
		return "", err
	}
	path, err := securejoin.SecureJoin(root, clean)
	if err != nil {
		return "", fmt.Errorf("%w: resolve workspace-relative path %q below %q: %w", chamberErrors.ErrInvalidRequest, rel, root, err)
	}
	return path, nil
}

func rejectSymlinkComponents(root string, cleanRel string) error {
	if cleanRel == "." {
		return nil
	}
	current := root
	for _, component := range strings.Split(cleanRel, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: workspace path %q contains symlink component %q", chamberErrors.ErrInvalidRequest, cleanRel, current)
			}
			continue
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: inspect workspace path component %q: %w", chamberErrors.ErrFilesystemFailed, current, err)
	}
	return nil
}

func absolutePath(label string, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("%w: %s is required", chamberErrors.ErrInvalidRequest, label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: resolve %s %q: %w", chamberErrors.ErrInvalidRequest, label, path, err)
	}
	return filepath.Clean(abs), nil
}

func (w *Workspace) mkdirPrivateAbs(path string, label string) error {
	info, err := w.ops.stat(path)
	if err == nil {
		return privateDirMetadata(path, info)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: read %s metadata %q: %w", chamberErrors.ErrFilesystemFailed, label, path, err)
	}
	if err := w.ops.mkdirAll(path, 0700); err != nil {
		return fmt.Errorf("%w: create %s %q: %w", chamberErrors.ErrFilesystemFailed, label, path, err)
	}
	info, err = w.ops.stat(path)
	if err != nil {
		return fmt.Errorf("%w: read %s metadata %q: %w", chamberErrors.ErrFilesystemFailed, label, path, err)
	}
	return privateDirMetadata(path, info)
}

func (w *Workspace) checkWorkspaceDir(path string, label string, requirePrivate bool) (bool, error) {
	info, err := w.ops.stat(path)
	if err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("%w: %q is not a directory", chamberErrors.ErrInvalidRequest, path)
		}
		if err := privateDirMetadata(path, info); err != nil {
			if requirePrivate {
				return false, err
			}
			return false, nil
		}
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("%w: read %s metadata %q: %w", chamberErrors.ErrFilesystemFailed, label, path, err)
	}
	if err := w.ops.mkdirAll(path, 0700); err != nil {
		return false, fmt.Errorf("%w: create %s %q: %w", chamberErrors.ErrFilesystemFailed, label, path, err)
	}
	info, err = w.ops.stat(path)
	if err != nil {
		return false, fmt.Errorf("%w: read %s metadata %q: %w", chamberErrors.ErrFilesystemFailed, label, path, err)
	}
	if err := privateDirMetadata(path, info); err != nil {
		if requirePrivate {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

func privateDirMetadata(path string, info os.FileInfo) error {
	if !info.IsDir() {
		return fmt.Errorf("%w: %q is not a directory", chamberErrors.ErrInvalidRequest, path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%w: path %q must not be readable, writable, or executable by group or other users", chamberErrors.ErrInvalidRequest, path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: cannot determine owner for private directory %q", chamberErrors.ErrFilesystemFailed, path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: private directory %q must be owned by the current user", chamberErrors.ErrInvalidRequest, path)
	}
	return nil
}

func (w *Workspace) probeCapabilities(privateDirs bool) (Capabilities, error) {
	probeRoot, err := os.MkdirTemp(w.root, ".hostfs-probe-*")
	if err != nil {
		return Capabilities{}, fmt.Errorf("%w: create host filesystem probe directory below %q: %w", chamberErrors.ErrFilesystemFailed, w.root, err)
	}
	defer w.ops.removeAll(probeRoot)

	caps := Capabilities{PrivateDirs: privateDirs}
	caps.FileFsync = w.probeFileFsync(probeRoot)
	caps.AtomicFileRename = w.probeFileRename(probeRoot, w.tmpRoot)
	caps.AtomicDirectoryRename = w.probeDirectoryRename(probeRoot, w.tmpRoot)
	if err := w.ops.syncDir(probeRoot); err == nil {
		caps.DirectoryFsync = true
	} else if !errors.Is(err, errDirectorySyncUnsupported) {
		return Capabilities{}, fmt.Errorf("%w: probe directory fsync below %q: %w", chamberErrors.ErrFilesystemFailed, probeRoot, err)
	}
	return caps, nil
}

func (w *Workspace) probeFileFsync(parent string) bool {
	path := filepath.Join(parent, "fsync-file")
	file, err := w.ops.openFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false
	}
	defer w.ops.remove(path)
	if _, err := file.Write([]byte("probe")); err != nil {
		_ = file.Close()
		return false
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false
	}
	return file.Close() == nil
}

func (w *Workspace) probeFileRename(rootProbe string, tmpRoot string) bool {
	return w.probeRenameFile(rootProbe, rootProbe) &&
		w.probeRenameFile(tmpRoot, tmpRoot) &&
		w.probeRenameFile(tmpRoot, rootProbe)
}

func (w *Workspace) probeRenameFile(srcParent string, dstParent string) bool {
	src, err := os.CreateTemp(srcParent, ".hostfs-file-src-*")
	if err != nil {
		return false
	}
	srcPath := src.Name()
	dstPath := filepath.Join(dstParent, ".hostfs-file-dst-"+filepath.Base(srcPath))
	defer w.ops.remove(srcPath)
	defer w.ops.remove(dstPath)
	if _, err := src.Write([]byte("probe")); err != nil {
		_ = src.Close()
		return false
	}
	if err := src.Close(); err != nil {
		return false
	}
	return w.ops.rename(srcPath, dstPath) == nil
}

func (w *Workspace) probeDirectoryRename(rootProbe string, tmpRoot string) bool {
	return w.probeRenameDirectory(rootProbe, rootProbe) &&
		w.probeRenameDirectory(tmpRoot, tmpRoot) &&
		w.probeRenameDirectory(tmpRoot, rootProbe)
}

func (w *Workspace) probeRenameDirectory(srcParent string, dstParent string) bool {
	srcPath, err := os.MkdirTemp(srcParent, ".hostfs-dir-src-*")
	if err != nil {
		return false
	}
	dstPath := filepath.Join(dstParent, ".hostfs-dir-dst-"+filepath.Base(srcPath))
	defer w.ops.removeAll(srcPath)
	defer w.ops.removeAll(dstPath)
	return w.ops.rename(srcPath, dstPath) == nil
}

func requireCapabilities(required Capabilities, observed Capabilities) error {
	if required.PrivateDirs && !observed.PrivateDirs {
		return fmt.Errorf("%w: host filesystem does not provide required private directory capability", chamberErrors.ErrFilesystemFailed)
	}
	if required.FileFsync && !observed.FileFsync {
		return fmt.Errorf("%w: host filesystem does not provide required file fsync capability", chamberErrors.ErrFilesystemFailed)
	}
	if required.DirectoryFsync && !observed.DirectoryFsync {
		return fmt.Errorf("%w: host filesystem does not provide required directory fsync capability", chamberErrors.ErrFilesystemFailed)
	}
	if required.AtomicFileRename && !observed.AtomicFileRename {
		return fmt.Errorf("%w: host filesystem does not provide required atomic file rename capability", chamberErrors.ErrFilesystemFailed)
	}
	if required.AtomicDirectoryRename && !observed.AtomicDirectoryRename {
		return fmt.Errorf("%w: host filesystem does not provide required atomic directory rename capability", chamberErrors.ErrFilesystemFailed)
	}
	return nil
}

func fillFilesystemOps(ops filesystemOps) filesystemOps {
	defaults := defaultFilesystemOps()
	if ops.mkdirAll == nil {
		ops.mkdirAll = defaults.mkdirAll
	}
	if ops.stat == nil {
		ops.stat = defaults.stat
	}
	if ops.openFile == nil {
		ops.openFile = defaults.openFile
	}
	if ops.rename == nil {
		ops.rename = defaults.rename
	}
	if ops.remove == nil {
		ops.remove = defaults.remove
	}
	if ops.removeAll == nil {
		ops.removeAll = defaults.removeAll
	}
	if ops.syncDir == nil {
		ops.syncDir = defaults.syncDir
	}
	return ops
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory %q: %w", path, err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EISDIR) {
			return errDirectorySyncUnsupported
		}
		return fmt.Errorf("sync directory %q: %w", path, err)
	}
	return nil
}
