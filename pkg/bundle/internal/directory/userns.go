package directory

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/subprocess"
	digest "github.com/opencontainers/go-digest"
	imageSpecs "github.com/opencontainers/image-spec/specs-go"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	ociConvert "github.com/opencontainers/umoci/oci/config/convert"
)

// Container root is the caller. Other container IDs use the caller's allocated
// subordinate IDs.
type userNamespace struct {
	uid []specs.LinuxIDMapping
	gid []specs.LinuxIDMapping
}

func currentUserNamespace() (userNamespace, error) {
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	ns := userNamespace{uid: []specs.LinuxIDMapping{{ContainerID: 0, HostID: uid, Size: 1}}, gid: []specs.LinuxIDMapping{{ContainerID: 0, HostID: gid, Size: 1}}}
	account, err := user.Current()
	name := ""
	if err == nil {
		name = account.Username
	}
	for _, item := range []struct {
		path string
		own  uint32
		maps *[]specs.LinuxIDMapping
	}{
		{"/etc/subuid", uid, &ns.uid}, {"/etc/subgid", gid, &ns.gid},
	} {
		data, err := os.ReadFile(item.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return ns, fmt.Errorf("%w: read %s: %w", chamberErrors.ErrUnsupportedHost, item.path, err)
		}
		mapping, err := subordinateRange(string(data), name, uid, item.own)
		if err != nil {
			return ns, fmt.Errorf("%w: %s: %w", chamberErrors.ErrUnsupportedHost, item.path, err)
		}
		if mapping != nil {
			*item.maps = append(*item.maps, *mapping)
		}
	}
	if (len(ns.uid) > 1) != (len(ns.gid) > 1) {
		return ns, fmt.Errorf("%w: configure both /etc/subuid and /etc/subgid for this user", chamberErrors.ErrUnsupportedHost)
	}
	return ns, nil
}

func subordinateRange(data, name string, accountID, ownID uint32) (*specs.LinuxIDMapping, error) {
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if (name == "" || fields[0] != name) && fields[0] != strconv.FormatUint(uint64(accountID), 10) {
			continue
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid subordinate ID entry %q", line)
		}
		start, startErr := strconv.ParseUint(fields[1], 10, 32)
		count, countErr := strconv.ParseUint(fields[2], 10, 32)
		if startErr != nil || countErr != nil || start == 0 || count == 0 || start+count > uint64(^uint32(0)) || (uint64(ownID) >= start && uint64(ownID) < start+count) {
			return nil, fmt.Errorf("invalid or overlapping subordinate ID range %q", line)
		}
		return &specs.LinuxIDMapping{ContainerID: 1, HostID: uint32(start), Size: uint32(count)}, nil
	}
	return nil, scanner.Err()
}

func (ns userNamespace) multipleIDs() bool { return len(ns.uid) > 1 && len(ns.gid) > 1 }

func (ns userNamespace) command(ctx context.Context, program string, args ...string) *exec.Cmd {
	// The comma syntax is accepted by both older and newer util-linux releases.
	flags := []string{"--user", "--map-root-user"}
	for _, item := range []struct {
		name string
		maps []specs.LinuxIDMapping
	}{{"users", ns.uid}, {"groups", ns.gid}} {
		for _, mapping := range item.maps[1:] {
			flags = append(flags, fmt.Sprintf("--map-%s=%d,%d,%d", item.name, mapping.HostID, mapping.ContainerID, mapping.Size))
		}
	}
	flags = append(flags, "--", program)
	flags = append(flags, args...)
	command := subprocess.CommandContext(ctx, "unshare", flags...)
	subprocess.SetProcessGroup(command)
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = 5 * time.Second
	return command
}

// The helper runs the same umoci version as the Go library, inside a mapped
// namespace. No host-root process, daemon, or application reexec hook is needed.
func (p *Provisioner) installUnpackHelper(ctx context.Context) (string, error) {
	const version = "v0.6.0"
	checksums := map[string]string{
		"amd64": "b51c267ec394499e42c6fde47f240b7b7dba57ea49df0b5acd304378b82a3b71",
		"arm64": "5cfd17f2e7a4bcf9ed67ea1b955ca893d200349b9ce6a3d3707dba415f458a1f",
	}
	checksum, ok := checksums[runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("%w: umoci helper does not support %s", chamberErrors.ErrUnsupportedHost, runtime.GOARCH)
	}
	// A leading dot keeps tool storage outside the valid bundle-ID names.
	binDir, err := p.workspace.MkdirPrivate(".bin")
	if err != nil {
		return "", fmt.Errorf("%w: create umoci binary directory: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	path := filepath.Join(binDir, "umoci-"+version+"-"+runtime.GOARCH)
	if file, err := os.Open(path); err == nil {
		hash := sha256.New()
		_, readErr := io.Copy(hash, file)
		closeErr := file.Close()
		if readErr == nil && closeErr == nil && hex.EncodeToString(hash.Sum(nil)) == checksum {
			return path, nil
		}
	}
	request, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/opencontainers/umoci/releases/download/"+version+"/umoci.linux."+runtime.GOARCH, nil)
	if err != nil {
		return "", fmt.Errorf("%w: install umoci helper: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("%w: download umoci helper: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: download umoci helper: HTTP %d", chamberErrors.ErrBundlePrepareFailed, response.StatusCode)
	}
	stage, err := p.workspace.CreateTemp(".bin", ".umoci-download-*")
	if err != nil {
		return "", fmt.Errorf("%w: install umoci helper: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	defer os.Remove(stage.Name())
	defer stage.Close()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(stage, hash), io.LimitReader(response.Body, 64<<20)); err != nil {
		return "", fmt.Errorf("%w: install umoci helper: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != checksum {
		return "", fmt.Errorf("%w: umoci helper checksum mismatch", chamberErrors.ErrBundlePrepareFailed)
	}
	if err := stage.Chmod(0700); err != nil {
		return "", fmt.Errorf("%w: install umoci helper: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	if err := stage.Sync(); err != nil {
		return "", fmt.Errorf("%w: install umoci helper: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	if err := stage.Close(); err != nil {
		return "", fmt.Errorf("%w: install umoci helper: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	if err := os.Rename(stage.Name(), path); err != nil {
		return "", fmt.Errorf("%w: install umoci helper: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	return path, nil
}

// Give the helper a private, unambiguous manifest while sharing immutable blobs.
// It cannot change the source image index or substitute a newly moved tag.
func (p *Provisioner) unpackMapped(ctx context.Context, layout, bundle string, manifest imagespec.Manifest) error {
	for _, helper := range []string{"unshare", "newuidmap", "newgidmap"} {
		if _, err := exec.LookPath(helper); err != nil {
			return fmt.Errorf("%w: mapped rootless bundles require %s: %w", chamberErrors.ErrUnsupportedHost, helper, err)
		}
	}
	helper, err := p.installUnpackHelper(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: install unpack helper: %w", chamberErrors.ErrCanceled, ctx.Err())
		}
		return err
	}
	view, err := p.workspace.MkdirTemp(".", ".unpack-image-*")
	if err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	defer os.RemoveAll(view)
	blobRoot := filepath.Join(view, "blobs", "sha256")
	if err := os.MkdirAll(blobRoot, 0700); err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	for _, descriptor := range append([]imagespec.Descriptor{manifest.Config}, manifest.Layers...) {
		if err := descriptor.Digest.Validate(); err != nil {
			return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
		}
		target := filepath.Join(view, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
		}
		source, err := filepath.Abs(filepath.Join(layout, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded()))
		if err != nil {
			return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
		}
		if err := os.Symlink(source, target); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	manifestDigest := digest.FromBytes(data)
	if err := os.WriteFile(filepath.Join(blobRoot, manifestDigest.Encoded()), data, 0600); err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	index := imagespec.Index{Versioned: imageSpecs.Versioned{SchemaVersion: 2}, Manifests: []imagespec.Descriptor{{MediaType: imagespec.MediaTypeImageManifest, Digest: manifestDigest, Size: int64(len(data)), Annotations: map[string]string{imagespec.AnnotationRefName: "unpack"}}}}
	data, err = json.Marshal(index)
	if err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	if err := os.WriteFile(filepath.Join(view, "index.json"), data, 0600); err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	if err := os.WriteFile(filepath.Join(view, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0600); err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	// Record the mappings before extraction so interrupted staging directories
	// remain removable even if runtime-config generation never finishes.
	stub := specs.Spec{Linux: &specs.Linux{UIDMappings: p.namespace.uid, GIDMappings: p.namespace.gid}}
	data, err = json.Marshal(stub)
	if err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	configPath := filepath.Join(bundle, "config.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	rootfs := filepath.Join(bundle, "rootfs")
	configStage := filepath.Join(bundle, ".config-next.json")
	for _, args := range [][]string{
		{"raw", "unpack", "--image", ".:unpack", rootfs},
		{"raw", "runtime-config", "--image", ".:unpack", "--rootfs", rootfs, configStage},
	} {
		command := p.namespace.command(ctx, helper, args...)
		command.Dir = view
		if output, err := command.CombinedOutput(); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%w: unpack mapped image: %w", chamberErrors.ErrCanceled, ctx.Err())
			}
			return fmt.Errorf("%w: unpack in rootless user namespace: %w: %s", chamberErrors.ErrBundlePrepareFailed, err, output)
		}
	}
	data, err = os.ReadFile(configStage)
	if err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	if err := ociConvert.ToRootless(&spec); err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	spec.Linux.UIDMappings = p.namespace.uid
	spec.Linux.GIDMappings = p.namespace.gid
	data, err = json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	if err := os.WriteFile(configStage, data, 0600); err != nil {
		return fmt.Errorf("%w: prepare mapped bundle: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	if err := os.Rename(configStage, configPath); err != nil {
		return fmt.Errorf("%w: publish mapped runtime config: %w", chamberErrors.ErrBundlePrepareFailed, err)
	}
	return nil
}

func removeBundleDirectory(ctx context.Context, path string) error {
	// Namespace setup can fail before extraction starts. Such staging directories
	// contain only caller-owned control files and need no mapped cleanup process.
	if _, err := os.Lstat(filepath.Join(path, "rootfs")); errors.Is(err, os.ErrNotExist) {
		return removeRuntimeMutatedBundle(ctx, path)
	} else if err != nil {
		return fmt.Errorf("%w: inspect bundle rootfs: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	data, err := os.ReadFile(filepath.Join(path, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return removeRuntimeMutatedBundle(ctx, path)
	}
	if err != nil {
		return fmt.Errorf("%w: read bundle namespace: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("%w: read bundle namespace: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if spec.Linux == nil {
		return removeRuntimeMutatedBundle(ctx, path)
	}
	ns := userNamespace{uid: spec.Linux.UIDMappings, gid: spec.Linux.GIDMappings}
	if !ns.multipleIDs() {
		return removeRuntimeMutatedBundle(ctx, path)
	}
	// Root in this namespace can remove files owned by any mapped container user,
	// including private or read-only directories. Paths are arguments, never shell code.
	command := ns.command(ctx, "/bin/sh", "-c", `set -e
chmod -R u+rwX -- "$1"
exec rm -rf --one-file-system -- "$1"`, "chamber-bundle-cleanup", path)
	if output, err := command.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: remove mapped bundle: %w", chamberErrors.ErrCanceled, ctx.Err())
		}
		return fmt.Errorf("%w: remove mapped bundle: %w: %s", chamberErrors.ErrFilesystemFailed, err, output)
	}
	return nil
}
