package runc

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

type testImageProvisioner struct {
	provision func(context.Context, chamberBundle.ProvisionRequest) (chamberBundle.ProvisionedBundle, error)
	remove    func(context.Context, chamberBundle.ProvisionedBundle) error
}

func (p testImageProvisioner) Descriptor() chamberBundle.Descriptor {
	return chamberBundle.Descriptor{Name: "test"}
}

func (p testImageProvisioner) Provision(ctx context.Context, request chamberBundle.ProvisionRequest) (chamberBundle.ProvisionedBundle, error) {
	return p.provision(ctx, request)
}

func (p testImageProvisioner) Remove(ctx context.Context, bundle chamberBundle.ProvisionedBundle) error {
	return p.remove(ctx, bundle)
}

func TestRunImageOwnsBundleAndPreservesLogs(t *testing.T) {
	logDir := privateTempDir(t)
	binaryPath := writeFakeRunc(t, logDir, `
case "$cmd" in
run)
	printf '%s' "$PWD" > "$logdir/bundle-path"
	cat
	printf 'image stderr' >&2
	exit 7
	;;
delete)
	printf 'container does not exist' >&2
	exit 1
	;;
*) exit 64 ;;
esac
`)
	rt := runtimeWithBinary(t, binaryPath, filepath.Join(privateTempDir(t), "runtime"))
	image := chamberImage.Image{
		LayoutPath: "/caller/images/layout",
		Reference:  "example.com/app:latest",
		Digest:     "sha256:" + strings.Repeat("a", 64),
		Platform:   chamberImage.Platform{OS: "linux", Architecture: "arm64"},
	}
	process := chamberBundle.ProcessSpec{Args: []string{"echo", "hello"}, Env: []string{"A=B"}, Cwd: "/work"}
	mounts := []chamberBundle.Mount{{Source: "/caller/work", Target: "/work"}}
	bundle := chamberBundle.ProvisionedBundle{BundleID: "provisioner-identity", BundlePath: runtimeBundlePath(t)}
	removed := 0
	rt.provisioner = testImageProvisioner{
		provision: func(ctx context.Context, req chamberBundle.ProvisionRequest) (chamberBundle.ProvisionedBundle, error) {
			if req.ImageLayout != image.LayoutPath || req.ImageRef != image.Reference || req.ImageDigest != image.Digest || req.ImagePlatform != image.Platform {
				t.Fatalf("provisioned wrong image: %#v", req)
			}
			if req.BundleID == "" || req.Process.Terminal == nil || *req.Process.Terminal {
				t.Fatalf("missing bundle identity or non-terminal default: %#v", req)
			}
			withoutTerminal := req.Process
			withoutTerminal.Terminal = nil
			if !reflect.DeepEqual(withoutTerminal, process) || !reflect.DeepEqual(req.Mounts, mounts) {
				t.Fatalf("process/mount overrides lost: %#v", req)
			}
			return bundle, nil
		},
		remove: func(ctx context.Context, actual chamberBundle.ProvisionedBundle) error {
			if actual != bundle {
				t.Fatalf("Remove received %#v, want returned bundle %#v", actual, bundle)
			}
			removed++
			return os.RemoveAll(actual.BundlePath)
		},
	}
	var stdout, stderr strings.Builder
	container, err := rt.RunImage(context.Background(), image, chamberRuntime.RunImageRequest{
		Process: process,
		Mounts:  mounts,
		Stdin:   strings.NewReader("image stdout"),
		Stdout:  []io.Writer{&stdout},
		Stderr:  []io.Writer{&stderr},
	})
	if err != nil {
		t.Fatal(err)
	}
	if container.ID() == "" {
		t.Fatal("missing generated container identity")
	}
	result, err := container.Wait(context.Background())
	if err != nil || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("Wait = %#v, %v, want exit 7", result, err)
	}
	if stdout.String() != "image stdout" || stderr.String() != "image stderr" {
		t.Fatalf("streamed output = %q/%q", stdout.String(), stderr.String())
	}
	if removed != 0 {
		t.Fatal("bundle removed before Delete")
	}
	if err := container.Delete(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("bundle removal calls = %d, want 1", removed)
	}
	if _, err := os.Stat(bundle.BundlePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bundle still exists: %v", err)
	}
	for _, stream := range []chamberRuntime.LogStream{chamberRuntime.StdoutLogStream, chamberRuntime.StderrLogStream} {
		if content, err := container.ReadLog(stream); err != nil || len(content) == 0 {
			t.Fatalf("log %s missing after bundle cleanup: %q, %v", stream, content, err)
		}
	}
	if err := container.Delete(context.Background(), true); err != nil {
		t.Fatalf("repeated Delete: %v", err)
	}
}

func TestRunImageRollsBackLaunchFailureWithUncanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanupErr := errors.New("bundle cleanup failed")
	bundle := chamberBundle.ProvisionedBundle{BundleID: "staged", BundlePath: runtimeBundlePath(t)}
	removed := false
	rt := &Runtime{provisioner: testImageProvisioner{
		provision: func(context.Context, chamberBundle.ProvisionRequest) (chamberBundle.ProvisionedBundle, error) {
			cancel()
			return bundle, nil
		},
		remove: func(ctx context.Context, actual chamberBundle.ProvisionedBundle) error {
			removed = true
			if ctx.Err() != nil || actual != bundle {
				t.Fatalf("cleanup context/bundle = %v/%#v", ctx.Err(), actual)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("launch rollback has no deadline")
			}
			return cleanupErr
		},
	}}
	_, err := rt.RunImage(ctx, chamberImage.Image{}, chamberRuntime.RunImageRequest{ContainerID: "requested-id"})
	if !removed || !errors.Is(err, chamberErrors.ErrCanceled) || !errors.Is(err, cleanupErr) {
		t.Fatalf("RunImage error = %v, removed=%v; want launch and cleanup failures", err, removed)
	}
}

func TestRunImageRollsBackRuntimeInitializationFailure(t *testing.T) {
	logDir := privateTempDir(t)
	if err := os.WriteFile(filepath.Join(logDir, "no-ack"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	binary := writeFakeRunc(t, logDir, `
case "$cmd" in
run) echo 'invalid mount' >&2; exit 1 ;;
delete) echo 'container does not exist' >&2; exit 1 ;;
esac
`)
	rt := runtimeWithBinary(t, binary, privateTempDir(t))
	bundle := chamberBundle.ProvisionedBundle{BundleID: "failed", BundlePath: runtimeBundlePath(t)}
	rt.provisioner = testImageProvisioner{
		provision: func(context.Context, chamberBundle.ProvisionRequest) (chamberBundle.ProvisionedBundle, error) {
			return bundle, nil
		},
		remove: func(ctx context.Context, b chamberBundle.ProvisionedBundle) error { return os.RemoveAll(b.BundlePath) },
	}
	_, err := rt.RunImage(context.Background(), chamberImage.Image{}, chamberRuntime.RunImageRequest{})
	if !errors.Is(err, chamberErrors.ErrRuntimeStartFailed) {
		t.Fatal(err)
	}
	if _, err := os.Stat(bundle.BundlePath); !os.IsNotExist(err) {
		t.Fatalf("launch failure retained the provisioned bundle: %v", err)
	}
}

func TestRunImageRejectsInvalidLaunchBeforeProvisioning(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		id   string
		want error
	}{
		{"nil context", nil, "valid", chamberErrors.ErrInvalidRequest},
		{"canceled", canceled, "valid", chamberErrors.ErrCanceled},
		{"invalid ID", context.Background(), "../unsafe", chamberErrors.ErrInvalidContainerID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &Runtime{provisioner: testImageProvisioner{
				provision: func(context.Context, chamberBundle.ProvisionRequest) (chamberBundle.ProvisionedBundle, error) {
					t.Fatal("provisioned an invalid launch")
					return chamberBundle.ProvisionedBundle{}, nil
				},
			}}
			_, err := rt.RunImage(tc.ctx, chamberImage.Image{}, chamberRuntime.RunImageRequest{ContainerID: tc.id})
			if !errors.Is(err, tc.want) {
				t.Fatalf("RunImage error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRunImagePreservesExplicitTerminalAndProvisioningError(t *testing.T) {
	terminal := true
	rt := &Runtime{provisioner: testImageProvisioner{
		provision: func(ctx context.Context, req chamberBundle.ProvisionRequest) (chamberBundle.ProvisionedBundle, error) {
			if req.Process.Terminal == nil || !*req.Process.Terminal {
				t.Fatal("explicit terminal override lost")
			}
			return chamberBundle.ProvisionedBundle{}, chamberErrors.ErrInvalidImageLayout
		},
		remove: func(context.Context, chamberBundle.ProvisionedBundle) error {
			t.Fatal("removed a bundle after failed provisioning")
			return nil
		},
	}}
	_, err := rt.RunImage(context.Background(), chamberImage.Image{}, chamberRuntime.RunImageRequest{
		Process: chamberBundle.ProcessSpec{Terminal: &terminal},
	})
	if !errors.Is(err, chamberErrors.ErrInvalidImageLayout) {
		t.Fatalf("RunImage error = %v, want layout error", err)
	}
}

func TestManagedBundleDeleteRetainsBundleOnControlFailureAndRetriesCleanup(t *testing.T) {
	logDir := privateTempDir(t)
	binaryPath := writeFakeRunc(t, logDir, `
case "$cmd" in
delete)
	if [ ! -f "$logdir/allow-delete" ]; then
		printf 'container still running' >&2
		exit 1
	fi
	;;
*) exit 64 ;;
esac
`)
	cleanupErr := errors.New("cleanup unavailable")
	attempts := 0
	container := &runcContainer{
		containerConfig: containerConfig{id: "managed", binaryPath: binaryPath, stateRoot: privateTempDir(t)},
		provisioner: testImageProvisioner{remove: func(context.Context, chamberBundle.ProvisionedBundle) error {
			attempts++
			if attempts == 1 {
				return cleanupErr
			}
			return nil
		}},
	}
	if err := container.Delete(context.Background(), false); !errors.Is(err, chamberErrors.ErrRuntimeControlFailed) || attempts != 0 {
		t.Fatalf("Delete = %v, cleanup attempts=%d; running bundle must be retained", err, attempts)
	}
	if err := os.WriteFile(filepath.Join(logDir, "allow-delete"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := container.Delete(context.Background(), true); !errors.Is(err, cleanupErr) {
		t.Fatalf("Delete = %v, want cleanup error", err)
	}
	if err := container.Delete(context.Background(), true); err != nil || attempts != 2 {
		t.Fatalf("retry Delete = %v, attempts=%d", err, attempts)
	}
}
