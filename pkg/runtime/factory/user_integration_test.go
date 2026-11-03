package factory_test

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
	"github.com/donglin-wang/chamber/pkg/shared/testutil"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	ggcrLayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestRunImageNonRootUserIntegration(t *testing.T) {
	if os.Getenv("CHAMBER_INTEGRATION") != "1" {
		t.Skip("set CHAMBER_INTEGRATION=1 for real rootless containers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := filepath.Join(t.TempDir(), "sdk")
	store, err := chamberImageFactory.NewStore(chamberImage.DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.Pull(ctx, chamberImage.PullRequest{Reference: "busybox:1.36.1"})
	if err != nil {
		t.Fatal(err)
	}
	layout, err := ggcrLayout.FromPath(base.LayoutPath)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := v1.NewHash(base.Digest)
	if err != nil {
		t.Fatal(err)
	}
	image, err := layout.Image(hash)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, entry := range []struct {
		path, body string
		uid, gid   int
		mode       int64
		kind       byte
	}{
		{"etc/passwd", "root:x:0:0:root:/root:/bin/sh\napp:x:1000:1234:app:/work:/bin/sh\n", 0, 0, 0644, tar.TypeReg},
		{"etc/group", "root:x:0:\napp:x:1234:\n", 0, 0, 0644, tar.TypeReg},
		{"work/", "", 1000, 1234, 0700, tar.TypeDir},
		{"work/owned", "private content", 1000, 1234, 0600, tar.TypeReg},
		{"work/module-cache/", "", 1000, 1234, 0555, tar.TypeDir},
		{"work/module-cache/go.mod", "module example.test/readonly\n", 1000, 1234, 0444, tar.TypeReg},
		{"root-only", "root private", 0, 0, 0600, tar.TypeReg},
	} {
		if err := writer.WriteHeader(&tar.Header{Name: entry.path, Uid: entry.uid, Gid: entry.gid, Mode: entry.mode, Typeflag: entry.kind, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	layer, err := tarball.LayerFromReader(bytes.NewReader(archive.Bytes()), tarball.WithMediaType(types.OCILayer))
	if err != nil {
		t.Fatal(err)
	}
	image, err = mutate.AppendLayers(image, layer)
	if err != nil {
		t.Fatal(err)
	}
	registry := testutil.NewFakeRegistry(t)
	config := chamberRuntime.DefaultConfig(root)
	rt, err := chamberRuntimeFactory.NewRuntime(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"app", "1000:1234"} {
		t.Run(user, func(t *testing.T) {
			cleanup := func(container chamberRuntime.Container) {
				t.Cleanup(func() {
					if err := container.Delete(context.Background(), true); err != nil {
						t.Error(err)
					}
				})
			}
			file, err := image.ConfigFile()
			if err != nil {
				t.Fatal(err)
			}
			file.Config.User = user
			file.Config.WorkingDir = "/work"
			file.Config.Cmd = []string{"/bin/sh", "-ec", `test "$(id -u)" = 1000
 test "$(id -g)" = 1234
 test "$(stat -c %u:%g /work/owned)" = 1000:1234
 test "$(cat /work/owned)" = 'private content'
 test "$(stat -c %a /work/module-cache)" = 555
 test "$(stat -c %a /work/module-cache/go.mod)" = 444
 test ! -w /work/module-cache
 cat /work/module-cache/go.mod
 test ! -r /root-only
 echo created > new
 test "$(stat -c %u:%g new)" = 1000:1234
 chmod 000 /work
 echo nonroot-ok`}
			configured, err := mutate.ConfigFile(image, file)
			if err != nil {
				t.Fatal(err)
			}
			ref := registry.Reference(t, "nonroot", "test")
			reference, err := name.ParseReference(ref)
			if err != nil {
				t.Fatal(err)
			}
			if err := remote.Write(reference, configured); err != nil {
				t.Fatal(err)
			}
			stored, err := store.Pull(ctx, chamberImage.PullRequest{Reference: ref, Policy: chamberImage.PullAlways})
			if err != nil {
				t.Fatal(err)
			}
			container, err := rt.RunImage(ctx, stored, chamberRuntime.RunImageRequest{})
			if err != nil {
				t.Fatal(err)
			}
			cleanup(container)
			result, err := container.Wait(ctx)
			stdout, _ := container.ReadLog(chamberRuntime.StdoutLogStream)
			stderr, _ := container.ReadLog(chamberRuntime.StderrLogStream)
			if err != nil || result.ExitCode == nil || *result.ExitCode != 0 || !strings.Contains(string(stdout), "nonroot-ok") {
				t.Fatalf("result=%+v err=%v stdout=%s stderr=%s", result, err, stdout, stderr)
			}
			if err := container.Delete(ctx, true); err != nil {
				t.Fatal(err)
			}
			// The private UID-1000 directory must not prevent cleanup from the host.
			bundles, err := filepath.Glob(filepath.Join(config.RuntimeRoot, "bundles", "image-*"))
			if err != nil || len(bundles) != 0 {
				t.Fatalf("bundles remain: %v %v", bundles, err)
			}
			// Explicit numeric overrides must use the same subordinate-ID mapping.
			uid, gid := uint32(2000), uint32(2345)
			container, err = rt.RunImage(ctx, stored, chamberRuntime.RunImageRequest{Process: chamberBundle.ProcessSpec{Cwd: "/", Args: []string{"/bin/sh", "-ec", "test $(id -u) = 2000; test $(id -g) = 2345"}, User: chamberBundle.ProcessUser{UID: &uid, GID: &gid}}})
			if err != nil {
				t.Fatal(err)
			}
			cleanup(container)
			result, err = container.Wait(ctx)
			if err != nil || result.ExitCode == nil || *result.ExitCode != 0 {
				stderr, _ := container.ReadLog(chamberRuntime.StderrLogStream)
				t.Fatalf("override: %+v %v %s", result, err, stderr)
			}
			if err := container.Delete(ctx, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}
