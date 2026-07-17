package puller

import (
	"runtime"
	"testing"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
)

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
