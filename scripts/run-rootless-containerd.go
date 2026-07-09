// This spike keeps its historical filename so the existing go run command
// continues to work, but it no longer starts or imports containerd.
package main

import (
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
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/umoci"
	"github.com/opencontainers/umoci/oci/layer"
)

const (
	runcVersion = "1.5.0"
	imageTag    = "pulled"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		usage()
		return nil
	}
	if runtime.GOOS != "linux" {
		return errors.New("this program must run on Linux; runc needs Linux namespaces")
	}
	if os.Geteuid() == 0 {
		return errors.New("refusing to run as root; this spike is for rootless execution")
	}
	if err := checkRootlessHost(); err != nil {
		return err
	}

	l, err := newLayout()
	if err != nil {
		return err
	}
	if err := l.mkdirAll(); err != nil {
		return err
	}
	runcPath, err := ensureRunc(l)
	if err != nil {
		return err
	}

	imageRef := "docker.io/library/alpine:latest"
	var command []string
	interactive := true
	if len(os.Args) > 1 {
		imageRef = os.Args[1]
	}
	if len(os.Args) > 2 {
		command = os.Args[2:]
		interactive = false
	}

	workdir, err := os.MkdirTemp(l.tmp, "run-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workdir)

	ociLayout := filepath.Join(workdir, "image")
	bundle := filepath.Join(workdir, "bundle")
	runcRoot := filepath.Join(workdir, "runc-state")
	containerID := fmt.Sprintf("chamber-rootless-%d", os.Getpid())

	fmt.Printf("Pulling %s with a Go registry client...\n", imageRef)
	if err := pullToOCILayout(context.Background(), imageRef, ociLayout); err != nil {
		return err
	}

	fmt.Println("Unpacking the OCI image layout into a rootless runtime bundle...")
	if err := unpackBundle(ociLayout, bundle); err != nil {
		return err
	}
	if err := patchRuntimeSpec(filepath.Join(bundle, "config.json"), command, interactive); err != nil {
		return err
	}

	fmt.Printf("Running %s as %s with rootless runc...\n", imageRef, containerID)
	return runRunc(runcPath, runcRoot, bundle, containerID)
}

func usage() {
	fmt.Fprint(os.Stdout, `Usage:
  go run scripts/run-rootless-containerd.go [IMAGE] [COMMAND...]

Examples:
  go run scripts/run-rootless-containerd.go
  go run scripts/run-rootless-containerd.go docker.io/library/busybox:latest echo hi

What this does:
  - Pulls an OCI image using a pure-Go registry client.
  - Writes the registry manifest, config, and layers into an OCI image layout.
  - Uses umoci as a Go library to create rootfs/ and config.json.
  - Downloads and verifies a pinned runc release when it is not cached.
  - Runs the resulting OCI runtime bundle as the current user, without sudo.
  - Defaults to docker.io/library/alpine:latest and its interactive /bin/sh.

Host requirements:
  - Linux with unprivileged user namespaces enabled.
  - Go and outbound HTTPS access. No preinstalled container tools are required.

Environment:
  CHAMBER_RUNC_SPIKE_HOME  Override the user-private storage directory.
  CHAMBER_RUNC_VERSION     Override runc version. Default: 1.5.0
`)
}

type spikeLayout struct {
	home      string
	bin       string
	downloads string
	tmp       string
}

func newLayout() (spikeLayout, error) {
	home := os.Getenv("CHAMBER_RUNC_SPIKE_HOME")
	if home == "" {
		dataHome := os.Getenv("XDG_DATA_HOME")
		if dataHome == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return spikeLayout{}, err
			}
			dataHome = filepath.Join(userHome, ".local", "share")
		}
		home = filepath.Join(dataHome, "chamber", "rootless-runc-spike")
	}

	home, err := filepath.Abs(home)
	if err != nil {
		return spikeLayout{}, err
	}
	return spikeLayout{
		home:      home,
		bin:       filepath.Join(home, "bin"),
		downloads: filepath.Join(home, "downloads"),
		tmp:       filepath.Join(home, "tmp"),
	}, nil
}

func (l spikeLayout) mkdirAll() error {
	for _, dir := range []string{l.home, l.bin, l.downloads, l.tmp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func pullToOCILayout(ctx context.Context, imageRef, destination string) error {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("parse image reference: %w", err)
	}

	platform := v1.Platform{
		OS:           "linux",
		Architecture: runtime.GOARCH,
	}
	image, err := remote.Image(
		ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(platform),
	)
	if err != nil {
		return fmt.Errorf("pull image: %w", err)
	}

	path, err := layout.Write(destination, empty.Index)
	if err != nil {
		return fmt.Errorf("create OCI image layout: %w", err)
	}
	if err := path.AppendImage(
		image,
		layout.WithAnnotations(map[string]string{
			"org.opencontainers.image.ref.name": imageTag,
		}),
		layout.WithPlatform(platform),
	); err != nil {
		return fmt.Errorf("write OCI image layout: %w", err)
	}
	return nil
}

func unpackBundle(ociLayout, bundle string) error {
	engine, err := umoci.OpenLayout(ociLayout)
	if err != nil {
		return fmt.Errorf("open OCI image layout: %w", err)
	}
	defer engine.Close()

	uid := uint32(os.Geteuid())
	gid := uint32(os.Getegid())
	mapOptions := layer.MapOptions{
		UIDMappings: []specs.LinuxIDMapping{{
			ContainerID: 0,
			HostID:      uid,
			Size:        1,
		}},
		GIDMappings: []specs.LinuxIDMapping{{
			ContainerID: 0,
			HostID:      gid,
			Size:        1,
		}},
		Rootless: true,
	}
	options := layer.UnpackOptions{
		OnDiskFormat: layer.DirRootfs{MapOptions: mapOptions},
	}
	if err := umoci.Unpack(engine, imageTag, bundle, options); err != nil {
		return fmt.Errorf("unpack OCI image: %w", err)
	}
	return nil
}

func patchRuntimeSpec(path string, command []string, interactive bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("decode runtime config: %w", err)
	}
	if spec.Process == nil || spec.Linux == nil {
		return errors.New("umoci produced an incomplete runtime config")
	}

	spec.Process.Terminal = interactive
	if len(command) > 0 {
		spec.Process.Args = command
	}

	spec.Linux.Namespaces = withoutNamespaces(
		spec.Linux.Namespaces,
		specs.NetworkNamespace,
		specs.CgroupNamespace,
	)
	spec.Linux.CgroupsPath = ""
	spec.Linux.Resources = nil
	spec.Mounts = withoutCgroupMounts(spec.Mounts)

	data, err = json.MarshalIndent(&spec, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return nil
}

func withoutNamespaces(namespaces []specs.LinuxNamespace, unwanted ...specs.LinuxNamespaceType) []specs.LinuxNamespace {
	blocked := make(map[specs.LinuxNamespaceType]bool, len(unwanted))
	for _, namespaceType := range unwanted {
		blocked[namespaceType] = true
	}
	filtered := namespaces[:0]
	for _, namespace := range namespaces {
		if !blocked[namespace.Type] {
			filtered = append(filtered, namespace)
		}
	}
	return filtered
}

func withoutCgroupMounts(mounts []specs.Mount) []specs.Mount {
	filtered := mounts[:0]
	for _, mount := range mounts {
		if mount.Type == "cgroup" || mount.Type == "cgroup2" || mount.Destination == "/sys/fs/cgroup" {
			continue
		}
		filtered = append(filtered, mount)
	}
	return filtered
}

func runRunc(runcPath, runcRoot, bundle, containerID string) error {
	if err := os.MkdirAll(runcRoot, 0o700); err != nil {
		return err
	}

	defer func() {
		cleanup := exec.Command(runcPath, "--root", runcRoot, "delete", "--force", containerID)
		_ = cleanup.Run()
	}()

	cmd := exec.Command(runcPath, "--root", runcRoot, "run", containerID)
	cmd.Dir = bundle
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func ensureRunc(l spikeLayout) (string, error) {
	version := envDefault("CHAMBER_RUNC_VERSION", runcVersion)
	arch, err := runcArch()
	if err != nil {
		return "", err
	}

	dst := filepath.Join(l.bin, "runc")
	checksums := filepath.Join(l.downloads, "runc-"+version+".sha256sum")
	checksumURL := fmt.Sprintf(
		"https://github.com/opencontainers/runc/releases/download/v%s/runc.sha256sum",
		version,
	)
	if !exists(checksums) {
		fmt.Printf("Downloading runc %s checksums...\n", version)
		if err := downloadFile(checksumURL, checksums, 0o600); err != nil {
			return "", err
		}
	}

	expected, err := expectedChecksum(checksums, "runc."+arch)
	if err != nil {
		return "", err
	}
	if exists(dst) {
		ok, err := fileHasChecksum(dst, expected)
		if err != nil {
			return "", err
		}
		if ok {
			if err := os.Chmod(dst, 0o755); err != nil {
				return "", err
			}
			return dst, nil
		}
	}

	url := fmt.Sprintf(
		"https://github.com/opencontainers/runc/releases/download/v%s/runc.%s",
		version,
		arch,
	)
	fmt.Printf("Downloading runc %s...\n", version)
	if err := downloadFile(url, dst, 0o755); err != nil {
		return "", err
	}
	ok, err := fileHasChecksum(dst, expected)
	if err != nil {
		return "", err
	}
	if !ok {
		_ = os.Remove(dst)
		return "", errors.New("downloaded runc binary did not match the release checksum")
	}
	return dst, nil
}

func expectedChecksum(path, filename string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimPrefix(fields[len(fields)-1], "*") == filename {
			if len(fields[0]) != sha256.Size*2 {
				break
			}
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("release checksums do not contain %s", filename)
}

func fileHasChecksum(path, expected string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return false, err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	return actual == strings.ToLower(expected), nil
}

func downloadFile(url, dst string, mode os.FileMode) error {
	tmp := dst + ".tmp"
	_ = os.Remove(tmp)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "chamber-rootless-runc-spike")

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}

	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func checkRootlessHost() error {
	checks := []struct {
		path string
		bad  string
		msg  string
	}{
		{
			path: "/proc/sys/kernel/unprivileged_userns_clone",
			bad:  "0",
			msg:  "unprivileged user namespaces are disabled",
		},
		{
			path: "/proc/sys/user/max_user_namespaces",
			bad:  "0",
			msg:  "this host allows zero user namespaces",
		},
		{
			path: "/proc/sys/kernel/apparmor_restrict_unprivileged_userns",
			bad:  "1",
			msg:  "AppArmor is restricting unprivileged user namespaces",
		},
	}
	for _, check := range checks {
		value, err := os.ReadFile(check.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(value)) == check.bad {
			return fmt.Errorf("%s; use a Linux host or VM configured for rootless containers", check.msg)
		}
	}
	return nil
}

func runcArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH, nil
	default:
		return "", fmt.Errorf("unsupported runc architecture %q", runtime.GOARCH)
	}
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
