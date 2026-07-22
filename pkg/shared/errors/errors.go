package errors

// Code is Chamber's stable, JSON-friendly error vocabulary. It also satisfies
// error so callers can use errors.Is with wrapped operation failures.
type Code string

func (c Code) Error() string {
	return string(c)
}

const (
	// ErrInvalidRequest means a request or config is missing required fields or
	// uses unsupported generic values.
	ErrInvalidRequest Code = "invalid_request"

	// ErrInvalidContainerID means a container ID is empty, unsafe, or outside
	// Chamber's accepted ID syntax.
	ErrInvalidContainerID Code = "invalid_container_id"

	// ErrInvalidImageReference means an image reference cannot be parsed or
	// canonicalized.
	ErrInvalidImageReference Code = "invalid_image_reference"

	// ErrInvalidImageLayout means an OCI image layout is missing, malformed, or
	// internally inconsistent.
	ErrInvalidImageLayout Code = "invalid_image_layout"

	// ErrInvalidBundleMount means a requested bundle mount is invalid or unsafe.
	ErrInvalidBundleMount Code = "invalid_bundle_mount"

	// ErrInvalidProcessSpec means requested process fields cannot be represented
	// in the bundle or runtime contract.
	ErrInvalidProcessSpec Code = "invalid_process_spec"

	// ErrCanceled means caller cancellation or deadline expiry stopped Chamber
	// work.
	ErrCanceled Code = "canceled"

	// ErrUnsupportedHost means the current host cannot run the requested
	// Chamber operation.
	ErrUnsupportedHost Code = "unsupported_host"

	// ErrFilesystemFailed means Chamber could not complete required filesystem
	// work.
	ErrFilesystemFailed Code = "filesystem_failed"

	// ErrImageNotFound means the requested image record or layout was not found.
	ErrImageNotFound Code = "image_not_found"

	// ErrPullFailed means image pull work failed after request validation.
	ErrPullFailed Code = "pull_failed"

	// ErrMetadataFailed means daemon metadata storage could not complete an
	// operation.
	ErrMetadataFailed Code = "metadata_failed"

	// ErrContainerNotFound means the requested container record or runtime state
	// was not found.
	ErrContainerNotFound Code = "container_not_found"

	// ErrLogNotFound means the requested container log stream was not found.
	ErrLogNotFound Code = "log_not_found"

	// ErrBundlePrepareFailed means bundle provisioning failed after request
	// validation.
	ErrBundlePrepareFailed Code = "bundle_prepare_failed"

	// ErrRuntimeInstallFailed means runtime artifact installation or validation
	// failed.
	ErrRuntimeInstallFailed Code = "runtime_install_failed"

	// ErrRuntimeStartFailed means a runtime failed to start a container.
	ErrRuntimeStartFailed Code = "runtime_start_failed"

	// ErrRuntimeControlFailed means a lifecycle control operation such as signal
	// or delete failed.
	ErrRuntimeControlFailed Code = "runtime_control_failed"

	// ErrRuntimeWaitFailed means waiting for a container failed for reasons
	// other than the process exit code.
	ErrRuntimeWaitFailed Code = "runtime_wait_failed"

	// ErrContainerExitNonzero means the container process exited with a non-zero
	// status.
	ErrContainerExitNonzero Code = "container_exit_nonzero"

	// ErrStateConflict means a requested metadata or lifecycle transition is not
	// valid from the current state.
	ErrStateConflict Code = "state_conflict"
)
