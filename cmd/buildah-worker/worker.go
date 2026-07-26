package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
	imageTypes "go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/unshare"
)

const requestEnv = "CHAMBER_BUILDAH_WORKER_REQUEST"

const protocolVersion = 1

type buildRequest struct {
	ProtocolVersion int               `json:"protocol_version"`
	Operation       string            `json:"operation,omitempty"`
	Reference       string            `json:"reference"`
	ContextPath     string            `json:"context_path"`
	DockerfilePath  string            `json:"dockerfile_path"`
	OutputArchive   string            `json:"output_archive"`
	GraphRoot       string            `json:"graph_root"`
	RunRoot         string            `json:"run_root"`
	StorageDriver   string            `json:"storage_driver"`
	Runtime         string            `json:"runtime,omitempty"`
	Isolation       string            `json:"isolation"`
	Platform        platform          `json:"platform"`
	Target          string            `json:"target,omitempty"`
	BuildArgs       map[string]string `json:"build_args,omitempty"`
}

type platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

type buildResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	Error           string `json:"error,omitempty"`
}

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr))
}

func run(stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	_ = unshare.IsRootless()
	if buildah.InitReexec() {
		return 0
	}

	content, fromEnv, err := requestBytes(stdin)
	if err != nil {
		return writeError(stderr, "read request: %v", err)
	}
	var request buildRequest
	if err := json.Unmarshal(content, &request); err != nil {
		return writeError(stderr, "decode request: %v", err)
	}
	if request.ProtocolVersion != protocolVersion {
		return writeResponse(stdout, buildResponse{
			ProtocolVersion: protocolVersion,
			Error:           fmt.Sprintf("unsupported protocol version %d", request.ProtocolVersion),
		}, stderr)
	}
	switch strings.TrimSpace(request.Operation) {
	case "probe":
		return writeResponse(stdout, buildResponse{ProtocolVersion: protocolVersion}, stderr)
	case "", "build":
	default:
		return writeResponse(stdout, buildResponse{
			ProtocolVersion: protocolVersion,
			Error:           fmt.Sprintf("unsupported operation %q", request.Operation),
		}, stderr)
	}
	if err := enterUserNamespace(content, fromEnv); err != nil {
		return writeError(stderr, "enter user namespace: %v", err)
	}
	if err := runBuild(context.Background(), request, stderr); err != nil {
		return writeResponse(stdout, buildResponse{ProtocolVersion: protocolVersion, Error: err.Error()}, stderr)
	}
	return writeResponse(stdout, buildResponse{ProtocolVersion: protocolVersion}, stderr)
}

func requestBytes(stdin io.Reader) ([]byte, bool, error) {
	if encoded := os.Getenv(requestEnv); encoded != "" {
		_ = os.Unsetenv(requestEnv)
		content, err := base64.StdEncoding.DecodeString(encoded)
		return content, true, err
	}
	content, err := io.ReadAll(stdin)
	if err != nil {
		return nil, false, err
	}
	if len(strings.TrimSpace(string(content))) == 0 {
		return content, false, nil
	}
	return content, false, nil
}

func enterUserNamespace(content []byte, fromEnv bool) error {
	if fromEnv {
		return nil
	}
	if err := os.Setenv(requestEnv, base64.StdEncoding.EncodeToString(content)); err != nil {
		return err
	}
	unshare.MaybeReexecUsingUserNamespace(false)
	_ = os.Unsetenv(requestEnv)
	return nil
}

func runBuild(ctx context.Context, request buildRequest, stderr io.Writer) error {
	isolation, err := parseIsolation(request.Isolation)
	if err != nil {
		return err
	}
	store, err := storage.GetStore(storage.StoreOptions{
		GraphRoot:       request.GraphRoot,
		RunRoot:         request.RunRoot,
		GraphDriverName: request.StorageDriver,
	})
	if err != nil {
		return fmt.Errorf("open Buildah storage: %w", err)
	}
	defer func() {
		if _, err := store.Shutdown(false); err != nil && !errors.Is(err, storage.ErrLayerUsedByContainer) {
			_, _ = fmt.Fprintf(stderr, "buildah-worker: shutdown Buildah storage: %v\n", err)
		}
	}()

	jobs := 1
	options := define.BuildOptions{
		Args:                    request.BuildArgs,
		CommonBuildOpts:         &define.CommonBuildOptions{HTTPProxy: true},
		ContextDirectory:        request.ContextPath,
		Err:                     stderr,
		ForceRmIntermediateCtrs: true,
		Isolation:               isolation,
		Jobs:                    &jobs,
		OS:                      request.Platform.OS,
		Output:                  "oci-archive:" + request.OutputArchive + ":" + request.Reference,
		OutputFormat:            define.OCIv1ImageManifest,
		Out:                     io.Discard,
		Platforms: []struct{ OS, Arch, Variant string }{
			{OS: request.Platform.OS, Arch: request.Platform.Architecture, Variant: request.Platform.Variant},
		},
		PullPolicy:             define.PullIfMissing,
		Quiet:                  true,
		RemoveIntermediateCtrs: true,
		ReportWriter:           io.Discard,
		Runtime:                request.Runtime,
		SystemContext: &imageTypes.SystemContext{
			ArchitectureChoice: request.Platform.Architecture,
			OSChoice:           request.Platform.OS,
			VariantChoice:      request.Platform.Variant,
		},
		Target: strings.TrimSpace(request.Target),
	}
	if _, _, err := imagebuildah.BuildDockerfiles(ctx, store, options, request.DockerfilePath); err != nil {
		return fmt.Errorf("build Dockerfile: %w", err)
	}
	return nil
}

func parseIsolation(value string) (define.Isolation, error) {
	switch strings.TrimSpace(value) {
	case "", "chroot":
		return define.IsolationChroot, nil
	case "oci":
		return define.IsolationOCI, nil
	case "rootless":
		return define.IsolationOCIRootless, nil
	default:
		return define.IsolationDefault, fmt.Errorf("unsupported Buildah isolation %q", value)
	}
}

func writeResponse(stdout io.Writer, response buildResponse, stderr io.Writer) int {
	if err := json.NewEncoder(stdout).Encode(response); err != nil {
		return writeError(stderr, "encode response: %v", err)
	}
	return 0
}

func writeError(stderr io.Writer, format string, args ...any) int {
	_, _ = io.WriteString(stderr, "buildah-worker: ")
	_, _ = fmt.Fprintf(stderr, format, args...)
	_, _ = io.WriteString(stderr, "\n")
	return 1
}
