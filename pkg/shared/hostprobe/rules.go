package hostprobe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"

	"github.com/donglin-wang/chamber/pkg/shared/subprocess"
)

const (
	CodeUnsupportedHostOS             = "unsupported_host_os"
	CodeRootlessRequiresNonRoot       = "rootless_requires_non_root"
	CodeUserNamespacesDisabled        = "user_namespaces_disabled"
	CodeUserNamespaceLimitUnavailable = "user_namespace_limit_unavailable"
	CodeAppArmorRestrictsUserns       = "apparmor_restricts_userns"
	CodeSubordinateIDMappingMissing   = "subordinate_id_mapping_missing"
	CodeHelperMissing                 = "helper_missing"
	CodeActiveProbeUnavailable        = "active_probe_unavailable"
	CodeActiveUserNamespaceFailed     = "active_user_namespace_failed"
)

var RequireLinux = Rule{
	name: "require_linux",
	run: func(_ context.Context, rule Rule) []string {
		if rule.goos() == "linux" {
			return nil
		}
		return []string{finding(
			CodeUnsupportedHostOS,
			fmt.Sprintf("Chamber host operation requires Linux; current GOOS is %q", rule.goos()),
			"Run this probe and the Chamber operation inside Linux.",
		)}
	},
}

var RequireRootlessUser = Rule{
	name: "require_rootless_user",
	run: func(_ context.Context, rule Rule) []string {
		if rule.euid() != 0 {
			return nil
		}
		return []string{finding(
			CodeRootlessRequiresNonRoot,
			"Chamber rootless operation cannot run as root",
			"Run Chamber as the unprivileged user that will own the Chamber roots.",
		)}
	},
}

var RequireUserNamespacesEnabled = Rule{
	name: "require_user_namespaces_enabled",
	run: func(_ context.Context, rule Rule) []string {
		var findings []string
		if value, ok, result := readHostInt(rule, "/proc/sys/kernel/unprivileged_userns_clone"); result != nil {
			findings = append(findings, *result)
		} else if ok && value == 0 {
			findings = append(findings, finding(
				CodeUserNamespacesDisabled,
				"/proc/sys/kernel/unprivileged_userns_clone is 0, so unprivileged user namespaces are disabled",
				"Enable unprivileged user namespaces for this host or run inside a Linux environment where they are allowed.",
			))
		}
		if value, ok, result := readHostInt(rule, "/proc/sys/user/max_user_namespaces"); result != nil {
			findings = append(findings, *result)
		} else if ok && value <= 0 {
			findings = append(findings, finding(
				CodeUserNamespacesDisabled,
				"/proc/sys/user/max_user_namespaces is 0, so this user cannot create user namespaces",
				"Set user.max_user_namespaces to a positive value appropriate for rootless containers.",
			))
		}
		return findings
	},
}

var RequireAppArmorAllowsUserNamespaces = Rule{
	name: "require_apparmor_allows_user_namespaces",
	run: func(_ context.Context, rule Rule) []string {
		var findings []string
		for _, path := range []string{
			"/proc/sys/kernel/apparmor_restrict_unprivileged_userns",
			"/proc/sys/kernel/apparmor_restrict_unprivileged_unconfined",
		} {
			value, ok, result := readHostInt(rule, path)
			if result != nil {
				findings = append(findings, *result)
				continue
			}
			if !ok || value == 0 {
				continue
			}
			findings = append(findings, finding(
				CodeAppArmorRestrictsUserns,
				fmt.Sprintf("%s is %d, so AppArmor may block unprivileged user namespace creation before runc init", path, value),
				"Use an AppArmor profile that permits Chamber's unprivileged user namespace setup, or have the host operator adjust this policy.",
			))
		}
		return findings
	},
}

var WarnSubUIDMapping = Rule{
	name: "warn_subuid_mapping",
	run: func(_ context.Context, rule Rule) []string {
		return checkSubordinateIDMapping(rule, "/etc/subuid")
	},
}

var WarnSubGIDMapping = Rule{
	name: "warn_subgid_mapping",
	run: func(_ context.Context, rule Rule) []string {
		return checkSubordinateIDMapping(rule, "/etc/subgid")
	},
}

var WarnNewUIDMap = Rule{
	name: "warn_newuidmap",
	run: func(_ context.Context, rule Rule) []string {
		return warnMissingHelper(rule, "newuidmap")
	},
}

var WarnNewGIDMap = Rule{
	name: "warn_newgidmap",
	run: func(_ context.Context, rule Rule) []string {
		return warnMissingHelper(rule, "newgidmap")
	},
}

var ProbeUserNamespace = Rule{
	name: "probe_user_namespace",
	run: func(ctx context.Context, rule Rule) []string {
		if _, err := rule.lookPath("unshare"); err != nil {
			return []string{finding(
				CodeActiveProbeUnavailable,
				"unshare was not found in PATH, so Chamber cannot actively verify user namespace policy",
				"Install util-linux unshare on the host used for active Chamber probes.",
			)}
		}
		output, err := rule.commandOutput(ctx, "unshare", "-Ur", "true")
		if err != nil {
			message := strings.TrimSpace(string(output))
			if message == "" {
				message = err.Error()
			}
			return []string{finding(
				CodeActiveUserNamespaceFailed,
				fmt.Sprintf("active user namespace probe failed: %s", message),
				"Fix user namespace or LSM/AppArmor policy before starting Chamber runtime or BuildKit work.",
			)}
		}
		return nil
	},
}

func checkSubordinateIDMapping(rule Rule, path string) []string {
	username, err := rule.currentUser()
	if err != nil || strings.TrimSpace(username) == "" {
		return []string{finding(
			CodeSubordinateIDMappingMissing,
			fmt.Sprintf("cannot determine current username while checking %s", path),
			"Verify the Chamber user has subordinate ID ranges when running images that need more than a single-ID rootless mapping.",
		)}
	}
	data, err := rule.readFile(path)
	if err != nil {
		return []string{finding(
			CodeSubordinateIDMappingMissing,
			fmt.Sprintf("%s is not readable for subordinate ID mapping checks: %v", path, err),
			"Add a subordinate ID range for the Chamber user when builds or images need multi-ID ownership mapping.",
		)}
	}
	prefix := username + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return nil
		}
	}
	return []string{finding(
		CodeSubordinateIDMappingMissing,
		fmt.Sprintf("%s does not contain a subordinate ID mapping for %q", path, username),
		"Add a subordinate ID range for the Chamber user when builds or images need multi-ID ownership mapping.",
	)}
}

func warnMissingHelper(rule Rule, name string) []string {
	if _, err := rule.lookPath(name); err == nil {
		return nil
	}
	return []string{finding(
		CodeHelperMissing,
		fmt.Sprintf("%s was not found in PATH", name),
		fmt.Sprintf("Install %s or configure Chamber to use an explicit equivalent helper path when that operation needs it.", name),
	)}
}

func readHostInt(rule Rule, path string) (int64, bool, *string) {
	data, err := rule.readFile(path)
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return 0, false, nil
		}
		result := finding(
			CodeUserNamespaceLimitUnavailable,
			fmt.Sprintf("cannot inspect %s: %v", path, err),
			"Verify this host's user namespace policy directly if Chamber operations fail later.",
		)
		return 0, false, &result
	}
	value := strings.TrimSpace(string(data))
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		result := finding(
			CodeUserNamespaceLimitUnavailable,
			fmt.Sprintf("cannot parse %s value %q: %v", path, value, err),
			"Fix the host sysctl value before running Chamber rootless operations.",
		)
		return 0, false, &result
	}
	return parsed, true, nil
}

func finding(code string, message string, remediation string) string {
	return fmt.Sprintf("%s: %s Remediation: %s", code, message, remediation)
}

func (r Rule) withDefaults() Rule {
	if r.goos == nil {
		r.goos = func() string { return runtime.GOOS }
	}
	if r.euid == nil {
		r.euid = os.Geteuid
	}
	if r.currentUser == nil {
		r.currentUser = currentUsername
	}
	if r.readFile == nil {
		r.readFile = os.ReadFile
	}
	if r.lookPath == nil {
		r.lookPath = exec.LookPath
	}
	if r.commandOutput == nil {
		r.commandOutput = commandOutput
	}
	return r
}

func currentUsername() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	return current.Username, nil
}

func commandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := subprocess.CommandContext(ctx, name, args...)
	return command.CombinedOutput()
}
