package runc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/donglin-wang/chamber/internal/fsutil"
	chruntime "github.com/donglin-wang/chamber/internal/runtime"
)

const (
	DefaultName    = "runc"
	DefaultVersion = "v1.5.0"

	defaultAMD64URL    = "https://github.com/opencontainers/runc/releases/download/v1.5.0/runc.amd64"
	defaultAMD64SHA256 = "0363e69bebd3a027d1239364ab9b4f4873f6bc4e7a7878e94b4ea59f08551297"
)

var _ chruntime.Runtime = (*Runtime)(nil)

type Config struct {
	RuntimeRoot   string
	RuntimeBinDir string
	Name          string
	Version       string
	URL           string
	SHA256        string
}

type Override struct {
	RuntimeRoot   *string
	RuntimeBinDir *string
	Name          *string
	Version       *string
	URL           *string
	SHA256        *string
}

type Runtime struct {
	config Config
	client *http.Client
}

type Option func(*Runtime)

func WithHTTPClient(client *http.Client) Option {
	return func(runtime *Runtime) {
		if client != nil {
			runtime.client = client
		}
	}
}

func DefaultConfig(rootPath string) Config {
	return Config{
		RuntimeRoot:   filepath.Join(rootPath, "run", "runtime"),
		RuntimeBinDir: filepath.Join(rootPath, "bin"),
		Name:          DefaultName,
	}
}

func Resolve(defaultConfig Config, override Override) (Config, error) {
	if override.RuntimeRoot != nil {
		defaultConfig.RuntimeRoot = *override.RuntimeRoot
	}
	if override.RuntimeBinDir != nil {
		defaultConfig.RuntimeBinDir = *override.RuntimeBinDir
	}
	if override.Name != nil {
		defaultConfig.Name = *override.Name
	}
	if override.Version != nil {
		defaultConfig.Version = *override.Version
	}
	if override.URL != nil {
		defaultConfig.URL = *override.URL
	}
	if override.SHA256 != nil {
		defaultConfig.SHA256 = *override.SHA256
	}

	var err error
	defaultConfig.RuntimeRoot, err = absPath(defaultConfig.RuntimeRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolve runtime root: %w", err)
	}
	defaultConfig.RuntimeBinDir, err = absPath(defaultConfig.RuntimeBinDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve runtime bin dir: %w", err)
	}

	return defaultConfig, nil
}

func New(config Config, options ...Option) *Runtime {
	if config.Name == "" {
		config.Name = DefaultName
	}
	if config.Version == "" {
		config.Version = DefaultVersion
	}
	if config.URL == "" && config.SHA256 == "" && goruntime.GOARCH == "amd64" {
		config.URL = defaultAMD64URL
		config.SHA256 = defaultAMD64SHA256
	}
	runtime := &Runtime{
		config: config,
		client: http.DefaultClient,
	}
	for _, option := range options {
		option(runtime)
	}

	return runtime
}

func (r *Runtime) Ensure(ctx context.Context) (chruntime.Binary, error) {
	config := r.config
	if config.Name == "" || config.Version == "" || config.URL == "" || config.SHA256 == "" {
		return chruntime.Binary{}, fmt.Errorf("runc runtime requires name, version, url, and sha256")
	}
	if config.RuntimeBinDir == "" {
		return chruntime.Binary{}, fmt.Errorf("runtime bin dir is required")
	}
	expectedDigest, err := parseSHA256(config.SHA256)
	if err != nil {
		return chruntime.Binary{}, err
	}

	binDir, err := absPath(config.RuntimeBinDir)
	if err != nil {
		return chruntime.Binary{}, fmt.Errorf("resolve runtime bin dir: %w", err)
	}
	if err := fsutil.EnsurePrivateDir(binDir); err != nil {
		return chruntime.Binary{}, fmt.Errorf("prepare runtime bin dir: %w", err)
	}

	binaryPath := filepath.Join(binDir, config.Name)
	if ok, err := fileMatchesSHA256(binaryPath, expectedDigest); err != nil {
		return chruntime.Binary{}, fmt.Errorf("verify existing runtime binary: %w", err)
	} else if ok {
		return chruntime.Binary{
			Name:    config.Name,
			Version: config.Version,
			Path:    binaryPath,
		}, nil
	}

	if err := r.download(ctx, config.URL, expectedDigest, binDir, binaryPath); err != nil {
		return chruntime.Binary{}, err
	}

	return chruntime.Binary{
		Name:    config.Name,
		Version: config.Version,
		Path:    binaryPath,
	}, nil
}

func (r *Runtime) Run(context.Context, chruntime.Binary, chruntime.RunRequest) (chruntime.StartResult, error) {
	return chruntime.StartResult{}, fmt.Errorf("runc Run is not implemented until challenge 9")
}

func (r *Runtime) download(ctx context.Context, url string, expectedDigest []byte, binDir string, binaryPath string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create runtime download request: %w", err)
	}

	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("download runtime binary: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download runtime binary: unexpected HTTP status %s", response.Status)
	}

	tmp, err := os.CreateTemp(binDir, "."+filepath.Base(binaryPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary runtime binary: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	digest := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, digest), response.Body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("download runtime binary: %w", err)
	}
	actualDigest := digest.Sum(nil)
	if !equalDigest(actualDigest, expectedDigest) {
		_ = tmp.Close()
		return fmt.Errorf("verify runtime binary checksum: got %s, want %s", hex.EncodeToString(actualDigest), hex.EncodeToString(expectedDigest))
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync runtime binary: %w", err)
	}
	if err := tmp.Chmod(0755); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set runtime binary mode: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close runtime binary: %w", err)
	}
	if err := os.Rename(tmpPath, binaryPath); err != nil {
		return fmt.Errorf("commit runtime binary: %w", err)
	}
	committed = true

	return nil
}

func fileMatchesSHA256(path string, expectedDigest []byte) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer file.Close()

	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return false, err
	}
	return equalDigest(digest.Sum(nil), expectedDigest), nil
}

func parseSHA256(raw string) ([]byte, error) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "sha256:")
	digest, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("parse runtime sha256: %w", err)
	}
	if len(digest) != sha256.Size {
		return nil, fmt.Errorf("parse runtime sha256: got %d bytes, want %d", len(digest), sha256.Size)
	}
	return digest, nil
}

func absPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	return filepath.Abs(path)
}

func equalDigest(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
