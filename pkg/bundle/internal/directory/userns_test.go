package directory

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestSubordinateRange(t *testing.T) {
	for _, test := range []struct {
		name, data, user string
		account, own     uint32
		want             *specs.LinuxIDMapping
		invalid          bool
	}{
		{name: "username", data: "other:200000:65536\napp:100000:65536\n", user: "app", account: 501, own: 501, want: &specs.LinuxIDMapping{ContainerID: 1, HostID: 100000, Size: 65536}},
		{name: "numeric uid for group allocation", data: "501:200000:65536", account: 501, own: 1000, want: &specs.LinuxIDMapping{ContainerID: 1, HostID: 200000, Size: 65536}},
		{name: "absent", data: "other:200000:65536", user: "app", account: 501, own: 501},
		{name: "empty username", data: ":200000:65536", account: 501, own: 501},
		{name: "zero", data: "app:0:65536", user: "app", account: 501, own: 501, invalid: true},
		{name: "empty range", data: "app:100000:0", user: "app", account: 501, own: 501, invalid: true},
		{name: "overlapping caller", data: "app:500:65536", user: "app", account: 501, own: 501, invalid: true},
		{name: "overflow", data: "app:4294901760:65536", user: "app", account: 501, own: 501, invalid: true},
		{name: "invalid number", data: "app:wrong:65536", user: "app", account: 501, own: 501, invalid: true},
		{name: "malformed", data: "app:100000", user: "app", account: 501, own: 501, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := subordinateRange(test.data, test.user, test.account, test.own)
			if (err != nil) != test.invalid || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("mapping=%+v error=%v; want %+v invalid=%t", got, err, test.want, test.invalid)
			}
		})
	}
}

func TestNamespaceCommandKeepsIdentityAndArguments(t *testing.T) {
	ns := userNamespace{
		uid: []specs.LinuxIDMapping{{ContainerID: 0, HostID: 501, Size: 1}, {ContainerID: 1, HostID: 100000, Size: 65536}},
		gid: []specs.LinuxIDMapping{{ContainerID: 0, HostID: 1000, Size: 1}, {ContainerID: 1, HostID: 200000, Size: 65536}},
	}
	command := ns.command(context.Background(), "/helper", "path with spaces; $(false)")
	expected := []string{"unshare", "--user", "--map-root-user", "--map-users=100000,1,65536", "--map-groups=200000,1,65536", "--", "/helper", "path with spaces; $(false)"}
	if !reflect.DeepEqual(command.Args, expected) {
		t.Fatalf("arguments: %#v", command.Args)
	}
	if command.SysProcAttr.Pdeathsig == 0 || !command.SysProcAttr.Setpgid {
		t.Fatal("namespace helper lost subprocess protection")
	}
}

func TestCleanupBeforeExtractionDoesNotNeedNamespaceHelper(t *testing.T) {
	root := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte("{incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeBundleDirectory(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("staging directory remains: %v", err)
	}
}

type helperTransport func(*http.Request) (*http.Response, error)

func (f helperTransport) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestUnpackHelperRejectsUnverifiedDownload(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundles")
	workspace := newTestWorkspace(t, root)
	provisioner := &Provisioner{config: chamberBundle.Config{Root: root}, workspace: workspace, client: &http.Client{Transport: helperTransport(func(*http.Request) (*http.Response, error) {
		entries, err := os.ReadDir(filepath.Join(root, ".bin"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("binary directory not created privately before download: %v %v", entries, err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("corrupt executable"))}, nil
	})}}
	if _, err := provisioner.installUnpackHelper(context.Background()); !errors.Is(err, chamberErrors.ErrBundlePrepareFailed) || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("download error: %v", err)
	}
	for _, directory := range []string{filepath.Join(root, ".bin"), filepath.Join(workspace.TmpRoot(), ".bin")} {
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 0 {
			t.Fatalf("unverified helper or staging file was retained: %v %v", entries, err)
		}
	}
}
