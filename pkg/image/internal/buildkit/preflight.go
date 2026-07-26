package buildkit

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

var runtimeGOOS = func() string {
	return runtime.GOOS
}

var runtimeArch = func() string {
	return runtime.GOARCH
}

var currentUID = os.Geteuid

func preflightInstallHost() error {
	if runtimeGOOS() != "linux" {
		return fmt.Errorf("%w: BuildKit rootless builds require Linux, got %s", chamberErrors.ErrUnsupportedHost, runtimeGOOS())
	}
	switch runtimeArch() {
	case "amd64", "arm64":
		return nil
	default:
		return fmt.Errorf("%w: BuildKit rootless builds do not have managed binaries for architecture %q", chamberErrors.ErrUnsupportedHost, runtimeArch())
	}
}

func preflightRootlessBuildHost() error {
	if err := preflightInstallHost(); err != nil {
		return err
	}
	if currentUID() == 0 {
		return fmt.Errorf("%w: BuildKit rootless builds must run as a non-root user", chamberErrors.ErrUnsupportedHost)
	}
	content, err := os.ReadFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return nil
		}
		return fmt.Errorf("%w: inspect unprivileged user namespace limit: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	value := strings.TrimSpace(string(content))
	namespaces, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: parse /proc/sys/user/max_user_namespaces value %q: %w", chamberErrors.ErrUnsupportedHost, value, err)
	}
	if namespaces == 0 {
		return fmt.Errorf("%w: /proc/sys/user/max_user_namespaces needs to be set to non-zero", chamberErrors.ErrUnsupportedHost)
	}
	return nil
}

func classifyUnsupportedHostMessage(message string) bool {
	message = strings.ToLower(message)
	for _, known := range []string{
		"/proc/sys/user/max_user_namespaces needs to be set to non-zero",
		"apparmor_restrict_unprivileged_userns is set to 1",
		"operation not permitted",
		"rootless mode requires to be executed as the mapped root in a user namespace",
	} {
		if strings.Contains(message, known) {
			return true
		}
	}
	return false
}
