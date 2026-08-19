package hostprobe

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRuleNameIsAccessibleThroughMethod(t *testing.T) {
	if got := RequireLinux.Name(); got != "require_linux" {
		t.Fatalf("Name() = %q, want require_linux", got)
	}
}

func TestRequireLinuxUsesStubbedGOOS(t *testing.T) {
	rule := RequireLinux
	rule.goos = func() string { return "darwin" }

	messages := rule.Check(context.Background())
	assertCheckContains(t, messages, CodeUnsupportedHostOS)
	assertCheckContains(t, messages, "darwin")
}

func TestRequireRootlessUserUsesStubbedEUID(t *testing.T) {
	rule := RequireRootlessUser
	rule.euid = func() int { return 0 }

	messages := rule.Check(context.Background())
	assertCheckContains(t, messages, CodeRootlessRequiresNonRoot)
}

func TestRequireUserNamespacesEnabledReportsDisabledLimits(t *testing.T) {
	rule := RequireUserNamespacesEnabled
	rule.readFile = func(path string) ([]byte, error) {
		switch path {
		case "/proc/sys/kernel/unprivileged_userns_clone":
			return []byte("0\n"), nil
		case "/proc/sys/user/max_user_namespaces":
			return []byte("0\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}

	messages := rule.Check(context.Background())
	if len(messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2: %#v", len(messages), messages)
	}
	assertCheckContains(t, messages, "/proc/sys/kernel/unprivileged_userns_clone")
	assertCheckContains(t, messages, "/proc/sys/user/max_user_namespaces")
}

func TestRequireAppArmorAllowsUserNamespacesReportsRestriction(t *testing.T) {
	rule := RequireAppArmorAllowsUserNamespaces
	rule.readFile = func(path string) ([]byte, error) {
		switch path {
		case "/proc/sys/kernel/apparmor_restrict_unprivileged_userns":
			return []byte("1\n"), nil
		case "/proc/sys/kernel/apparmor_restrict_unprivileged_unconfined":
			return []byte("0\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}

	messages := rule.Check(context.Background())
	assertCheckContains(t, messages, CodeAppArmorRestrictsUserns)
	assertCheckContains(t, messages, "apparmor_restrict_unprivileged_userns")
}

func TestRequireSubUIDMappingUsesStubbedUserAndFile(t *testing.T) {
	rule := RequireSubUIDMapping
	rule.currentUser = func() (string, error) { return "tester", nil }
	rule.readFile = func(string) ([]byte, error) {
		return []byte("other:100000:65536\n"), nil
	}

	messages := rule.Check(context.Background())
	assertCheckContains(t, messages, CodeSubordinateIDMappingMissing)
	assertCheckContains(t, messages, "tester")
}

func TestRequireNewUIDMapUsesStubbedLookPath(t *testing.T) {
	rule := RequireNewUIDMap
	rule.lookPath = func(string) (string, error) {
		return "", errors.New("not found")
	}

	messages := rule.Check(context.Background())
	assertCheckContains(t, messages, CodeHelperMissing)
	assertCheckContains(t, messages, "newuidmap")
}

func TestProbeUserNamespaceUsesStubbedCommand(t *testing.T) {
	rule := ProbeUserNamespace
	rule.lookPath = func(string) (string, error) {
		return "/usr/bin/unshare", nil
	}
	rule.commandOutput = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("unshare: Operation not permitted"), errors.New("exit status 1")
	}

	messages := rule.Check(context.Background())
	assertCheckContains(t, messages, CodeActiveUserNamespaceFailed)
	assertCheckContains(t, messages, "Operation not permitted")
}

func TestRulesReturnNoMessagesWhenStubbedHostPasses(t *testing.T) {
	rule := RequireUserNamespacesEnabled
	rule.readFile = func(path string) ([]byte, error) {
		return []byte("1024\n"), nil
	}

	if messages := rule.Check(context.Background()); len(messages) != 0 {
		t.Fatalf("messages = %#v, want none", messages)
	}
}

func assertCheckContains(t *testing.T, messages []string, want string) {
	t.Helper()
	for _, message := range messages {
		if strings.Contains(message, want) {
			return
		}
	}
	t.Fatalf("no check message contains %q: %#v", want, messages)
}
