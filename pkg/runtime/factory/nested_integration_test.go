package factory_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberBundleFactory "github.com/donglin-wang/chamber/pkg/bundle/factory"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
)

// Both launch paths use Chamber's nesting-ready provisioning defaults.
func TestNestedContainersIntegration(t *testing.T) {
	if os.Getenv("CHAMBER_INTEGRATION") != "1" {
		t.Skip("set CHAMBER_INTEGRATION=1 for real rootless containers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := filepath.Join(t.TempDir(), "sdk")
	source := t.TempDir()
	// End the inner allocation below 65,535 so this test also fits inside
	// Dockerfile.test's package-test container mapping.
	dockerfile := `FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends uidmap util-linux libcap2-bin \
 && useradd --create-home --uid 1000 chamber \
 && printf 'chamber:2000:60000\n' > /etc/subuid \
 && printf 'chamber:2000:60000\n' > /etc/subgid \
 && chmod 0755 /usr/bin/newuidmap /usr/bin/newgidmap \
 && setcap cap_setuid=ep /usr/bin/newuidmap \
 && setcap cap_setgid=ep /usr/bin/newgidmap
COPY --chmod=0644 check-nested.sh /check-nested.sh
USER chamber
WORKDIR /home/chamber
CMD ["sh", "/check-nested.sh"]
`
	script := `set -eu
test "$(id -u)" = 1000
unshare --user --map-root-user --map-users=2000,1,60000 --map-groups=2000,1,60000 --mount --pid --fork sh -ec '
 mount -t proc proc /proc
 test "$(id -u)" = 0
 test "$(id -g)" = 0
 test "$(wc -l < /proc/self/uid_map)" = 2
 test "$(wc -l < /proc/self/gid_map)" = 2
 mkdir locked
 touch locked/owned
 chown 60000:60000 locked/owned
 test "$(stat -c %u:%g locked/owned)" = 60000:60000
 chmod 000 locked
 echo nested-containers-ok
'
`
	for name, data := range map[string]string{"Dockerfile.test": dockerfile, "check-nested.sh": script} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := chamberImageFactory.NewStore(chamberImage.DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	image, err := store.Build(ctx, chamberImage.BuildRequest{
		Reference: "chamber.local/nested:test", ContextPath: source, DockerfilePath: filepath.Join(source, "Dockerfile.test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := chamberRuntimeFactory.NewRuntime(ctx, chamberRuntime.DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"RunImage", "Run"} {
		t.Run(method, func(t *testing.T) {
			var container chamberRuntime.Container
			var provisioner chamberBundle.Provisioner
			var ready chamberBundle.ProvisionedBundle
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if container != nil {
					if err := container.Delete(cleanupCtx, true); err != nil {
						t.Error(err)
						return
					}
				}
				if ready.BundleID != "" {
					if err := provisioner.Remove(cleanupCtx, ready); err != nil {
						t.Error(err)
					}
				}
			})
			var err error
			if method == "RunImage" {
				container, err = rt.RunImage(ctx, image, chamberRuntime.RunImageRequest{})
			} else {
				provisioner, err = chamberBundleFactory.NewProvisioner(chamberBundle.DefaultConfig(filepath.Join(root, "caller")))
				if err != nil {
					t.Fatal(err)
				}
				terminal := false
				ready, err = provisioner.Provision(ctx, chamberBundle.ProvisionRequest{
					BundleID: "nested", ImageLayout: image.LayoutPath, ImageRef: image.Reference,
					ImageDigest: image.Digest, ImagePlatform: image.Platform, Process: chamberBundle.ProcessSpec{Terminal: &terminal},
				})
				if err != nil {
					t.Fatal(err)
				}
				container, err = rt.Run(ctx, chamberRuntime.RunRequest{ContainerID: "nested", BundlePath: ready.BundlePath})
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := container.Wait(ctx)
			stdout, _ := container.ReadLog(chamberRuntime.StdoutLogStream)
			stderr, _ := container.ReadLog(chamberRuntime.StderrLogStream)
			if err != nil || result.ExitCode == nil || *result.ExitCode != 0 || !strings.Contains(string(stdout), "nested-containers-ok") {
				t.Fatalf("result=%+v error=%v stdout=%s stderr=%s", result, err, stdout, stderr)
			}
			if err := container.Delete(ctx, true); err != nil {
				t.Fatal(err)
			}
			t.Log("mapped subordinate UIDs/GIDs, mounted nested proc, and removed private mapped files")
		})
	}
}
