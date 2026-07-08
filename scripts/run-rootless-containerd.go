package main

import (
	"archive/tar"
	"compress/gzip"
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
)

const (
	containerdVersion  = "2.3.2"
	rootlessKitVersion = "2.3.1"
	runcVersion        = "1.5.0"
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
		return errors.New("this script must run on Linux; containerd/runc need Linux namespaces")
	}
	if os.Geteuid() == 0 {
		return errors.New("refusing to run as root; this spike is for rootless execution")
	}
	if err := checkRootlessHost(); err != nil {
		return err
	}

	layout, err := newLayout()
	if err != nil {
		return err
	}
	if err := layout.mkdirAll(); err != nil {
		return err
	}
	if err := ensureTools(layout); err != nil {
		return err
	}

	image := "docker.io/library/alpine:latest"
	command := []string{"/bin/sh"}
	interactive := true
	if len(os.Args) > 1 {
		image = os.Args[1]
	}
	if len(os.Args) > 2 {
		command = os.Args[2:]
		interactive = false
	}

	containerID := fmt.Sprintf("chamber-rootless-%d", os.Getpid())
	platform := "linux/" + runtime.GOARCH
	return runContainerdPipeline(layout, platform, image, containerID, command, interactive)
}

func usage() {
	fmt.Fprint(os.Stdout, `Usage:
  go run scripts/run-rootless-containerd.go [IMAGE] [COMMAND...]

Examples:
  go run scripts/run-rootless-containerd.go
  go run scripts/run-rootless-containerd.go docker.io/library/busybox:latest echo hi

What this does:
  - Downloads containerd, ctr, containerd-shim-runc-v2, runc, and rootlesskit.
  - Stores them under a user-private Chamber spike directory.
  - Starts containerd through rootlesskit without sudo.
  - Uses the downloaded ctr to pull the image and run a one-shot container.
  - Defaults to docker.io/library/alpine:latest and an interactive /bin/sh.

Environment:
  CHAMBER_CONTAINERD_SPIKE_HOME  Override the storage directory.
  CHAMBER_CONTAINERD_VERSION     Override containerd version. Default: 2.3.2
  CHAMBER_ROOTLESSKIT_VERSION    Override RootlessKit version. Default: 2.3.1
  CHAMBER_RUNC_VERSION           Override runc version. Default: 1.5.0
`)
}

type layout struct {
	home             string
	bin              string
	downloads        string
	containerdRoot   string
	containerdState  string
	tmp              string
	rootlessKitState string
	socket           string
	fifo             string
	log              string
	runcRoot         string
}

func newLayout() (layout, error) {
	home := os.Getenv("CHAMBER_CONTAINERD_SPIKE_HOME")
	if home == "" {
		dataHome := os.Getenv("XDG_DATA_HOME")
		if dataHome == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return layout{}, err
			}
			dataHome = filepath.Join(userHome, ".local", "share")
		}
		home = filepath.Join(dataHome, "chamber", "rootless-containerd-spike")
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return layout{}, err
	}

	runBase := os.Getenv("XDG_RUNTIME_DIR")
	if runBase == "" {
		runBase = filepath.Join(home, "run")
	}
	runBase = filepath.Join(runBase, "chamber-rootless-containerd-spike")

	return layout{
		home:             home,
		bin:              filepath.Join(home, "bin"),
		downloads:        filepath.Join(home, "downloads"),
		containerdRoot:   filepath.Join(home, "containerd-root"),
		containerdState:  filepath.Join(runBase, "containerd-state"),
		tmp:              filepath.Join(home, "tmp"),
		rootlessKitState: filepath.Join(runBase, "rootlesskit-state"),
		socket:           filepath.Join(runBase, "containerd.sock"),
		fifo:             filepath.Join(runBase, "fifo"),
		log:              filepath.Join(runBase, "containerd.log"),
		runcRoot:         filepath.Join(runBase, "runc-root"),
	}, nil
}

func (l layout) mkdirAll() error {
	for _, dir := range []string{
		l.home,
		l.bin,
		l.downloads,
		l.containerdRoot,
		l.containerdState,
		l.tmp,
		l.rootlessKitState,
		l.fifo,
		l.runcRoot,
		filepath.Dir(l.socket),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func ensureTools(l layout) error {
	if err := ensureContainerd(l); err != nil {
		return err
	}
	if err := ensureRootlessKit(l); err != nil {
		return err
	}
	if err := ensureRunc(l); err != nil {
		return err
	}
	return nil
}

func ensureContainerd(l layout) error {
	if exists(filepath.Join(l.bin, "containerd")) &&
		exists(filepath.Join(l.bin, "ctr")) &&
		exists(filepath.Join(l.bin, "containerd-shim-runc-v2")) {
		return nil
	}

	version := envDefault("CHAMBER_CONTAINERD_VERSION", containerdVersion)
	arch, err := containerdArch()
	if err != nil {
		return err
	}
	url := fmt.Sprintf(
		"https://github.com/containerd/containerd/releases/download/v%s/containerd-%s-linux-%s.tar.gz",
		version,
		version,
		arch,
	)
	dst := filepath.Join(l.downloads, fmt.Sprintf("containerd-%s-linux-%s.tar.gz", version, arch))
	fmt.Printf("Downloading containerd %s...\n", version)
	if err := downloadFile(url, dst, 0o600); err != nil {
		return err
	}
	return extractTarGz(dst, l.bin, func(name string) bool {
		base := filepath.Base(name)
		return strings.HasPrefix(name, "bin/") && (base == "containerd" || base == "ctr" || strings.HasPrefix(base, "containerd-shim"))
	})
}

func ensureRootlessKit(l layout) error {
	if exists(filepath.Join(l.bin, "rootlesskit")) {
		return nil
	}

	version := envDefault("CHAMBER_ROOTLESSKIT_VERSION", rootlessKitVersion)
	arch, err := rootlessKitArch()
	if err != nil {
		return err
	}
	url := fmt.Sprintf(
		"https://github.com/rootless-containers/rootlesskit/releases/download/v%s/rootlesskit-%s.tar.gz",
		version,
		arch,
	)
	dst := filepath.Join(l.downloads, fmt.Sprintf("rootlesskit-%s-%s.tar.gz", version, arch))
	fmt.Printf("Downloading RootlessKit %s...\n", version)
	if err := downloadFile(url, dst, 0o600); err != nil {
		return err
	}
	return extractTarGz(dst, l.bin, func(name string) bool {
		base := filepath.Base(name)
		return base == "rootlesskit" || base == "rootlessctl"
	})
}

func ensureRunc(l layout) error {
	if exists(filepath.Join(l.bin, "runc")) {
		return nil
	}

	version := envDefault("CHAMBER_RUNC_VERSION", runcVersion)
	arch, err := runcArch()
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://github.com/opencontainers/runc/releases/download/v%s/runc.%s", version, arch)
	dst := filepath.Join(l.bin, "runc")
	fmt.Printf("Downloading runc %s...\n", version)
	return downloadFile(url, dst, 0o755)
}

func runContainerdPipeline(l layout, platform, image, containerID string, command []string, interactive bool) error {
	rootlesskit := filepath.Join(l.bin, "rootlesskit")
	ttyFlag := ""
	if interactive {
		ttyFlag = "--tty"
	}
	script := `
set -eu

image="$1"
platform="$2"
container_id="$3"
tty_flag="$4"
shift 4

export PATH=` + shellQuote(l.bin) + `:$PATH
export TMPDIR=` + shellQuote(l.tmp) + `

mkdir -p ` + shellQuote(l.containerdRoot) + ` ` + shellQuote(l.containerdState) + ` ` + shellQuote(l.fifo) + ` ` + shellQuote(l.runcRoot) + `
rm -f ` + shellQuote(l.socket) + `

containerd \
  --log-level info \
  --root ` + shellQuote(l.containerdRoot) + ` \
  --state ` + shellQuote(l.containerdState) + ` \
  --address ` + shellQuote(l.socket) + ` \
  >` + shellQuote(l.log) + ` 2>&1 &

containerd_pid="$!"
cleanup() {
  kill "$containerd_pid" >/dev/null 2>&1 || true
  wait "$containerd_pid" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

i=0
while [ ! -S ` + shellQuote(l.socket) + ` ]; do
  i=$((i + 1))
  if [ "$i" -gt 100 ]; then
    echo "containerd did not create its socket in time" >&2
    echo "containerd log:" >&2
    cat ` + shellQuote(l.log) + ` >&2 || true
    exit 1
  fi
  sleep 0.1
done

echo "Pulling $image with rootless containerd..."
ctr --address ` + shellQuote(l.socket) + ` --namespace chamber images pull --local --platform "$platform" --snapshotter native "$image"

echo "Running $image as $container_id..."
ctr --address ` + shellQuote(l.socket) + ` --namespace chamber run \
  --rm \
  $tty_flag \
  --snapshotter native \
  --fifo-dir ` + shellQuote(l.fifo) + ` \
  --cgroup= \
  --cpu-shares 0 \
  --runc-root ` + shellQuote(l.runcRoot) + ` \
  "$image" "$container_id" "$@"
`

	args := []string{
		"--state-dir", l.rootlessKitState,
		"--net=host",
		"--copy-up=/etc",
		"--copy-up=/run",
		"/bin/sh",
		"-c",
		script,
		"chamber-containerd-pipeline",
		image,
		platform,
		containerID,
		ttyFlag,
	}
	args = append(args, command...)

	fmt.Printf("Starting rootless containerd through RootlessKit...\n")
	fmt.Printf("State root: %s\n", l.home)
	fmt.Printf("Runtime root: %s\n", filepath.Dir(l.socket))
	cmd := exec.Command(rootlesskit, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), "PATH="+l.bin+":"+os.Getenv("PATH"))
	return cmd.Run()
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
			msg:  "unprivileged user namespaces are disabled; try a Linux VM with user namespaces enabled",
		},
		{
			path: "/proc/sys/user/max_user_namespaces",
			bad:  "0",
			msg:  "this host allows zero user namespaces; try a Linux VM with user namespaces enabled",
		},
		{
			path: "/proc/sys/kernel/apparmor_restrict_unprivileged_userns",
			bad:  "1",
			msg:  "AppArmor is restricting unprivileged user namespaces; use a permissive local VM or configure an AppArmor policy",
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
			return errors.New(check.msg)
		}
	}
	return nil
}

func downloadFile(url, dst string, mode os.FileMode) error {
	if exists(dst) {
		return os.Chmod(dst, mode)
	}

	tmp := dst + ".tmp"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "chamber-rootless-containerd-spike")

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
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func extractTarGz(src, dst string, include func(string) bool) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	extracted := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || !include(hdr.Name) {
			continue
		}

		target := filepath.Join(dst, filepath.Base(hdr.Name))
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		if err := os.Chmod(target, 0o755); err != nil {
			return err
		}
		extracted++
	}
	if extracted == 0 {
		return fmt.Errorf("no matching binaries found in %s", src)
	}
	return nil
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

func containerdArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH, nil
	default:
		return "", fmt.Errorf("unsupported containerd arch %q", runtime.GOARCH)
	}
}

func runcArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH, nil
	default:
		return "", fmt.Errorf("unsupported runc arch %q", runtime.GOARCH)
	}
}

func rootlessKitArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64", nil
	case "arm64":
		return "aarch64", nil
	default:
		return "", fmt.Errorf("unsupported RootlessKit arch %q", runtime.GOARCH)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
