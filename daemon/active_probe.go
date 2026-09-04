package main

import (
	"archive/tar"
	"bytes"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	chamberSubprocess "github.com/donglin-wang/chamber/pkg/shared/subprocess"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	ggcrLayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

const (
	daemonActiveProbeCommand = "active-probe"
	daemonActiveProbeID      = "daemon-active-probe"
	daemonActiveProbeMarker  = "chamber-daemon-active-probe-ok"
	daemonActiveProbeRef     = "chamber.local/daemon-active-probe:latest"
)

type daemonActiveProbeResult struct {
	RecoveryErr  error
	ProvisionErr error
	RunErr       error
	CleanupErr   error
}

func runDaemonActiveProbes(
	ctx context.Context,
	tmpRoot string,
	executablePath string,
	provisioner chamberBundle.Provisioner,
	rt chamberRuntime.Runtime,
) (result daemonActiveProbeResult) {
	probeRoot := filepath.Join(tmpRoot, daemonActiveProbeID)
	if err := reclaimAbandonedDaemonActiveProbe(provisioner, rt, probeRoot); err != nil {
		result.RecoveryErr = err
		return result
	}
	layoutRoot := filepath.Join(probeRoot, "image")
	defer func() {
		result.CleanupErr = errors.Join(result.CleanupErr, os.RemoveAll(probeRoot))
	}()

	imageDigest, err := writeDaemonActiveProbeLayout(layoutRoot, executablePath)
	if err != nil {
		result.ProvisionErr = err
		return result
	}
	terminal := false
	bundle, err := provisioner.Provision(ctx, chamberBundle.ProvisionRequest{
		ContainerID:   daemonActiveProbeID,
		ImageLayout:   layoutRoot,
		ImageRef:      daemonActiveProbeRef,
		ImageDigest:   imageDigest,
		ImagePlatform: chamberImage.Platform{OS: "linux", Architecture: runtime.GOARCH},
		Process: chamberBundle.ProcessSpec{
			Args:     []string{"/chamber-active-probe", daemonActiveProbeCommand},
			Terminal: &terminal,
		},
	})
	if err != nil {
		result.ProvisionErr = fmt.Errorf("provision daemon active probe: %w", err)
		return result
	}
	defer func() {
		result.CleanupErr = errors.Join(result.CleanupErr, provisioner.Remove(context.Background(), bundle))
	}()

	result.RunErr = runDaemonActiveProbeContainer(ctx, rt, bundle)
	return result
}

func reclaimAbandonedDaemonActiveProbe(
	provisioner chamberBundle.Provisioner,
	rt chamberRuntime.Runtime,
	probeRoot string,
) error {
	var recoveryErr error
	container, err := rt.Open(context.Background(), daemonActiveProbeID)
	if err == nil {
		recoveryErr = errors.Join(recoveryErr, cleanupDaemonActiveProbeContainer(container, true))
	} else if !looksAlreadyDeleted(err) {
		recoveryErr = errors.Join(recoveryErr, fmt.Errorf("open abandoned daemon active probe container: %w", err))
	}
	recoveryErr = errors.Join(recoveryErr, provisioner.Remove(context.Background(), chamberBundle.ProvisionedBundle{
		ContainerID: daemonActiveProbeID,
	}))
	recoveryErr = errors.Join(recoveryErr, os.RemoveAll(probeRoot))
	if recoveryErr != nil {
		return fmt.Errorf("reclaim abandoned daemon active probe: %w", recoveryErr)
	}
	return nil
}

func writeDaemonActiveProbeLayout(layoutRoot string, executablePath string) (string, error) {
	layerFiles, err := daemonActiveProbeLayerFiles(executablePath)
	if err != nil {
		return "", err
	}
	var archive bytes.Buffer
	tarWriter := tar.NewWriter(&archive)
	for _, layerFile := range layerFiles {
		if err := appendDaemonActiveProbeLayerFile(tarWriter, layerFile); err != nil {
			_ = tarWriter.Close()
			return "", err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return "", fmt.Errorf("close daemon active probe layer: %w", err)
	}

	image, err := mutate.AppendLayers(empty.Image, static.NewLayer(archive.Bytes(), types.OCIUncompressedLayer))
	if err != nil {
		return "", fmt.Errorf("create daemon active probe image: %w", err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		return "", fmt.Errorf("read daemon active probe image config: %w", err)
	}
	config.OS = "linux"
	config.Architecture = runtime.GOARCH
	config.Config.Entrypoint = []string{"/chamber-active-probe", daemonActiveProbeCommand}
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		return "", fmt.Errorf("write daemon active probe image config: %w", err)
	}
	image = mutate.MediaType(image, types.OCIManifestSchema1)
	image = mutate.ConfigMediaType(image, types.OCIConfigJSON)

	layoutPath, err := ggcrLayout.Write(layoutRoot, mutate.IndexMediaType(empty.Index, types.OCIImageIndex))
	if err != nil {
		return "", fmt.Errorf("create daemon active probe OCI layout: %w", err)
	}
	if err := layoutPath.AppendImage(
		image,
		ggcrLayout.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}),
		ggcrLayout.WithAnnotations(map[string]string{"org.opencontainers.image.ref.name": daemonActiveProbeRef}),
	); err != nil {
		return "", fmt.Errorf("append daemon active probe image: %w", err)
	}
	digest, err := image.Digest()
	if err != nil {
		return "", fmt.Errorf("read daemon active probe image digest: %w", err)
	}
	return digest.String(), nil
}

type daemonActiveProbeLayerFile struct {
	source string
	target string
}

func daemonActiveProbeLayerFiles(executablePath string) ([]daemonActiveProbeLayerFile, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("%w: daemon active probe requires Linux, got %s", chamberErrors.ErrUnsupportedHost, runtime.GOOS)
	}
	files := []daemonActiveProbeLayerFile{{source: executablePath, target: "/chamber-active-probe"}}
	executable, err := elf.Open(executablePath)
	if err != nil {
		return nil, fmt.Errorf("inspect daemon executable linkage: %w", err)
	}
	defer executable.Close()
	interpreter := ""
	for _, program := range executable.Progs {
		if program.Type != elf.PT_INTERP {
			continue
		}
		content, err := io.ReadAll(program.Open())
		if err != nil {
			return nil, fmt.Errorf("read daemon ELF interpreter: %w", err)
		}
		interpreter = strings.TrimRight(string(content), "\x00")
		break
	}
	if interpreter == "" {
		return files, nil
	}
	output, err := chamberSubprocess.Command(interpreter, "--list", executablePath).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list daemon shared libraries with %q: %w: %s", interpreter, err, strings.TrimSpace(string(output)))
	}
	paths := map[string]struct{}{interpreter: {}}
	for _, field := range strings.Fields(string(output)) {
		if filepath.IsAbs(field) {
			paths[field] = struct{}{}
		}
	}
	orderedPaths := make([]string, 0, len(paths))
	for path := range paths {
		orderedPaths = append(orderedPaths, path)
	}
	sort.Strings(orderedPaths)
	for _, path := range orderedPaths {
		files = append(files, daemonActiveProbeLayerFile{source: path, target: path})
	}
	return files, nil
}

func appendDaemonActiveProbeLayerFile(tarWriter *tar.Writer, layerFile daemonActiveProbeLayerFile) error {
	file, err := os.Open(layerFile.source)
	if err != nil {
		return fmt.Errorf("open daemon active probe layer file %q: %w", layerFile.source, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("inspect daemon active probe layer file %q: %w", layerFile.source, err)
	}
	mode := int64(info.Mode().Perm())
	if layerFile.target == "/chamber-active-probe" {
		mode = 0o555
	}
	if err := tarWriter.WriteHeader(&tar.Header{
		Name:     strings.TrimPrefix(filepath.Clean(layerFile.target), "/"),
		Mode:     mode,
		Size:     info.Size(),
		Typeflag: tar.TypeReg,
		ModTime:  time.Unix(0, 0).UTC(),
	}); err != nil {
		_ = file.Close()
		return fmt.Errorf("write daemon active probe layer header for %q: %w", layerFile.target, err)
	}
	_, copyErr := io.Copy(tarWriter, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("write daemon active probe layer file %q: %w", layerFile.target, err)
	}
	return nil
}

func runDaemonActiveProbeContainer(ctx context.Context, rt chamberRuntime.Runtime, bundle chamberBundle.ProvisionedBundle) (probeErr error) {
	container, err := rt.Run(ctx, chamberRuntime.RunRequest{Bundle: bundle})
	if err != nil {
		return fmt.Errorf("run daemon active probe container: %w", err)
	}
	defer func() {
		probeErr = errors.Join(probeErr, cleanupDaemonActiveProbeContainer(container, false))
	}()

	state, err := waitForDaemonActiveProbeState(ctx, container)
	if err != nil {
		result, waitErr := container.Wait(ctx)
		stderr, _ := container.ReadLog(chamberRuntime.StderrLogStream)
		return fmt.Errorf("read daemon active probe runtime state: %w; wait result=%#v wait error=%v stderr=%q", err, result, waitErr, strings.TrimSpace(string(stderr)))
	}
	if state.ContainerID != container.ID() {
		return fmt.Errorf("daemon active probe runtime state container id = %q, want %q", state.ContainerID, container.ID())
	}
	result, err := container.Wait(ctx)
	if err != nil {
		return fmt.Errorf("wait for daemon active probe container: %w", err)
	}
	if result.Status != chamberRuntime.ContainerResultStatusExited || result.ExitCode == nil || *result.ExitCode != 0 {
		return fmt.Errorf("daemon active probe result = %#v, want exited with code 0", result)
	}
	stdout, err := container.ReadLog(chamberRuntime.StdoutLogStream)
	if err != nil {
		return fmt.Errorf("read daemon active probe stdout: %w", err)
	}
	if !strings.Contains(string(stdout), daemonActiveProbeMarker) {
		return fmt.Errorf("daemon active probe stdout omitted marker %q", daemonActiveProbeMarker)
	}
	if _, err := container.ReadLog(chamberRuntime.StderrLogStream); err != nil {
		return fmt.Errorf("read daemon active probe stderr: %w", err)
	}
	return nil
}

func waitForDaemonActiveProbeState(ctx context.Context, container chamberRuntime.ContainerHandle) (chamberRuntime.ContainerState, error) {
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for {
		state, err := container.State(ctx)
		if err == nil {
			return state, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return chamberRuntime.ContainerState{}, lastErr
		}
		select {
		case <-ctx.Done():
			return chamberRuntime.ContainerState{}, errors.Join(lastErr, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func cleanupDaemonActiveProbeContainer(container chamberRuntime.ContainerHandle, tolerateMissing bool) error {
	if container == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cleanupErr error
	if err := container.Delete(ctx, true); err != nil && (!tolerateMissing || !looksAlreadyDeleted(err)) {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete daemon active probe container: %w", err))
	}
	for _, stream := range []chamberRuntime.LogStream{chamberRuntime.StdoutLogStream, chamberRuntime.StderrLogStream} {
		if err := container.DeleteLog(stream); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete daemon active probe %s log: %w", stream, err))
		}
	}
	return cleanupErr
}
