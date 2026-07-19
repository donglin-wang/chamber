package puller

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

func TestNewPreparesConfiguredImageRoot(t *testing.T) {
	root := filepath.Join(privateTempDir(t), "images")

	puller, err := New(chamberImage.Config{Root: root}, localfs.NewDirectoryManager())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if puller == nil {
		t.Fatal("New() puller = nil, want puller")
	}
	assertPrivateDir(t, root)
}

func TestNewRequiresConfiguredImageRoot(t *testing.T) {
	if _, err := New(chamberImage.Config{}, localfs.NewDirectoryManager()); err == nil {
		t.Fatal("New() error = nil, want root required error")
	}
}

func TestNewRequiresDirectoryManager(t *testing.T) {
	if _, err := New(chamberImage.Config{}, nil); err == nil {
		t.Fatal("New() error = nil, want directory manager error")
	}
}

func TestResolvePlatformDefaultsToLinuxHostArchitecture(t *testing.T) {
	platform := resolvePlatform(chamberImage.Platform{})

	if platform.OS != "linux" {
		t.Fatalf("OS = %q, want linux", platform.OS)
	}
	if platform.Architecture != runtime.GOARCH {
		t.Fatalf("Architecture = %q, want %q", platform.Architecture, runtime.GOARCH)
	}
	if platform.Variant != "" {
		t.Fatalf("Variant = %q, want empty", platform.Variant)
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()

	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatalf("Chmod(%q) error = %v", path, err)
	}
	return path
}

func assertPrivateDir(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", path)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("mode = %o, want 0700", info.Mode().Perm())
	}
}

func TestResolvePlatformAppliesRequestFields(t *testing.T) {
	platform := resolvePlatform(chamberImage.Platform{
		OS:           "linux",
		Architecture: "arm64",
		Variant:      "v8",
	})

	if platform.OS != "linux" || platform.Architecture != "arm64" || platform.Variant != "v8" {
		t.Fatalf("platform = %#v, want linux/arm64/v8", platform)
	}
}

func TestAuthenticatorAppliesBasicAndTokenAuth(t *testing.T) {
	auth, err := authenticator(&chamberImage.Auth{
		Username: "user",
		Password: "pass",
		Token:    "registry-token",
	}).Authorization()
	if err != nil {
		t.Fatalf("Authorization() error = %v", err)
	}

	if auth.Username != "user" || auth.Password != "pass" || auth.RegistryToken != "registry-token" {
		t.Fatalf("auth config = %#v, want username/password/token", auth)
	}
}
