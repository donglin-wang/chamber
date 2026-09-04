package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
)

func TestDaemonActiveProbesProvisionRunReadLogsAndCleanup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("daemon active probe requires Linux")
	}
	executablePath, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() error = %v", err)
	}
	provisioner := &activeProbeProvisioner{bundleRoot: t.TempDir()}
	abandonedContainer := &activeProbeContainer{id: daemonActiveProbeID}
	container := &activeProbeContainer{id: daemonActiveProbeID}
	result := runDaemonActiveProbes(
		context.Background(),
		t.TempDir(),
		executablePath,
		provisioner,
		&activeProbeRuntime{openContainer: abandonedContainer, runContainer: container},
	)
	if err := errors.Join(result.RecoveryErr, result.ProvisionErr, result.RunErr, result.CleanupErr); err != nil {
		t.Fatalf("runDaemonActiveProbes() error = %v", err)
	}
	if provisioner.request.ImageRef != daemonActiveProbeRef || len(provisioner.request.Process.Args) != 2 {
		t.Fatalf("provision request = %#v", provisioner.request)
	}
	if provisioner.removeCalls != 2 || !abandonedContainer.deleted || !container.deleted || len(container.deletedLogs) != 2 {
		t.Fatalf("cleanup: provisioner remove calls=%d abandoned deleted=%v container deleted=%v deleted logs=%v", provisioner.removeCalls, abandonedContainer.deleted, container.deleted, container.deletedLogs)
	}
}

func TestDaemonActiveProbeReportsRuntimeFailureAndStillCleansBundle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("daemon active probe requires Linux")
	}
	executablePath, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() error = %v", err)
	}
	provisioner := &activeProbeProvisioner{bundleRoot: t.TempDir()}
	runFailure := errors.New("injected runtime probe failure")
	result := runDaemonActiveProbes(
		context.Background(),
		t.TempDir(),
		executablePath,
		provisioner,
		&activeProbeRuntime{openContainer: &activeProbeContainer{id: daemonActiveProbeID}, runErr: runFailure},
	)
	if result.RecoveryErr != nil || result.ProvisionErr != nil || !errors.Is(result.RunErr, runFailure) || result.CleanupErr != nil {
		t.Fatalf("active probe result = %#v", result)
	}
	if provisioner.removeCalls != 2 {
		t.Fatal("provisioned active probe bundle was not removed after runtime failure")
	}
}

func TestDaemonActiveProbeReclaimsAbandonedDeterministicProbeBeforeStarting(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("daemon active probe requires Linux")
	}
	executablePath, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() error = %v", err)
	}
	tmpRoot := t.TempDir()
	probeRoot := filepath.Join(tmpRoot, daemonActiveProbeID)
	abandonedLayoutMarker := filepath.Join(probeRoot, "image", "abandoned")
	if err := os.MkdirAll(filepath.Dir(abandonedLayoutMarker), 0o700); err != nil {
		t.Fatalf("MkdirAll(abandoned layout) error = %v", err)
	}
	if err := os.WriteFile(abandonedLayoutMarker, []byte("stale"), 0o600); err != nil {
		t.Fatalf("WriteFile(abandoned layout marker) error = %v", err)
	}
	bundleRoot := t.TempDir()
	abandonedBundle := filepath.Join(bundleRoot, daemonActiveProbeID)
	if err := os.MkdirAll(abandonedBundle, 0o700); err != nil {
		t.Fatalf("MkdirAll(abandoned bundle) error = %v", err)
	}
	provisioner := &activeProbeProvisioner{
		bundleRoot:  bundleRoot,
		staleMarker: abandonedLayoutMarker,
	}
	abandonedContainer := &activeProbeContainer{id: daemonActiveProbeID}
	container := &activeProbeContainer{id: daemonActiveProbeID}
	runtime := &activeProbeRuntime{openContainer: abandonedContainer, runContainer: container}

	result := runDaemonActiveProbes(context.Background(), tmpRoot, executablePath, provisioner, runtime)
	if err := errors.Join(result.RecoveryErr, result.ProvisionErr, result.RunErr, result.CleanupErr); err != nil {
		t.Fatalf("runDaemonActiveProbes() error = %v", err)
	}
	if runtime.openedID != daemonActiveProbeID || !abandonedContainer.deleted {
		t.Fatalf("abandoned runtime cleanup: opened ID=%q deleted=%v", runtime.openedID, abandonedContainer.deleted)
	}
	if provisioner.staleMarkerAtProvision {
		t.Fatal("Provision observed abandoned layout marker after preflight cleanup")
	}
	if provisioner.removeCalls != 2 {
		t.Fatalf("provisioner Remove calls = %d, want preflight and final cleanup", provisioner.removeCalls)
	}
	if _, err := os.Stat(probeRoot); !os.IsNotExist(err) {
		t.Fatalf("active probe root stat error = %v, want not exist", err)
	}
}

type activeProbeProvisioner struct {
	bundleRoot             string
	request                chamberBundle.ProvisionRequest
	removeCalls            int
	staleMarker            string
	staleMarkerAtProvision bool
}

func (p *activeProbeProvisioner) Descriptor() chamberBundle.Descriptor {
	return chamberBundle.Descriptor{Name: "active-probe-fake"}
}

func (p *activeProbeProvisioner) Provision(ctx context.Context, request chamberBundle.ProvisionRequest) (chamberBundle.ProvisionedBundle, error) {
	if err := ctx.Err(); err != nil {
		return chamberBundle.ProvisionedBundle{}, err
	}
	if err := chamberImage.ValidateLayoutContext(ctx, request.ImageLayout); err != nil {
		return chamberBundle.ProvisionedBundle{}, err
	}
	if p.staleMarker != "" {
		if _, err := os.Stat(p.staleMarker); err == nil {
			p.staleMarkerAtProvision = true
		} else if !os.IsNotExist(err) {
			return chamberBundle.ProvisionedBundle{}, err
		}
	}
	p.request = request
	bundlePath := filepath.Join(p.bundleRoot, request.ContainerID)
	if err := os.MkdirAll(bundlePath, 0o700); err != nil {
		return chamberBundle.ProvisionedBundle{}, err
	}
	return chamberBundle.ProvisionedBundle{ContainerID: request.ContainerID, BundlePath: bundlePath}, nil
}

func (p *activeProbeProvisioner) Remove(_ context.Context, bundle chamberBundle.ProvisionedBundle) error {
	p.removeCalls++
	bundlePath := bundle.BundlePath
	if bundlePath == "" {
		bundlePath = filepath.Join(p.bundleRoot, bundle.ContainerID)
	}
	return os.RemoveAll(bundlePath)
}

type activeProbeRuntime struct {
	openContainer chamberRuntime.ContainerHandle
	runContainer  chamberRuntime.Container
	openedID      string
	runErr        error
}

func (r *activeProbeRuntime) Descriptor() chamberRuntime.Descriptor {
	return chamberRuntime.Descriptor{Name: "active-probe-fake"}
}

func (r *activeProbeRuntime) Run(context.Context, chamberRuntime.RunRequest) (chamberRuntime.Container, error) {
	return r.runContainer, r.runErr
}

func (r *activeProbeRuntime) Open(_ context.Context, id string) (chamberRuntime.ContainerHandle, error) {
	r.openedID = id
	return r.openContainer, nil
}

type activeProbeContainer struct {
	id          string
	deleted     bool
	deletedLogs []chamberRuntime.LogStream
}

func (c *activeProbeContainer) ID() string                              { return c.id }
func (c *activeProbeContainer) StdoutPath() string                      { return "/stdout" }
func (c *activeProbeContainer) StderrPath() string                      { return "/stderr" }
func (c *activeProbeContainer) Signal(context.Context, os.Signal) error { return nil }

func (c *activeProbeContainer) State(context.Context) (chamberRuntime.ContainerState, error) {
	return chamberRuntime.ContainerState{ContainerID: c.id, Status: chamberRuntime.ContainerStatusRunning}, nil
}

func (c *activeProbeContainer) Wait(context.Context) (chamberRuntime.ContainerResult, error) {
	exitCode := 0
	return chamberRuntime.ContainerResult{
		ContainerID: c.id,
		Status:      chamberRuntime.ContainerResultStatusExited,
		ExitCode:    &exitCode,
		ExitedAt:    time.Now().UTC(),
	}, nil
}

func (c *activeProbeContainer) Delete(context.Context, bool) error {
	c.deleted = true
	return nil
}

func (c *activeProbeContainer) ReadLog(stream chamberRuntime.LogStream) ([]byte, error) {
	if stream == chamberRuntime.StdoutLogStream {
		return []byte(daemonActiveProbeMarker + "\n"), nil
	}
	return nil, nil
}

func (c *activeProbeContainer) DeleteLog(stream chamberRuntime.LogStream) error {
	c.deletedLogs = append(c.deletedLogs, stream)
	return nil
}
