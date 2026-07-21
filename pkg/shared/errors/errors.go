package errors

// Code is Chamber's stable, JSON-friendly error vocabulary. It also satisfies
// error so callers can use errors.Is with wrapped operation failures.
type Code string

func (c Code) Error() string {
	return string(c)
}

const (
	ErrInvalidRequest        Code = "invalid_request"
	ErrInvalidContainerID    Code = "invalid_container_id"
	ErrInvalidImageReference Code = "invalid_image_reference"
	ErrInvalidImageLayout    Code = "invalid_image_layout"
	ErrInvalidBundleMount    Code = "invalid_bundle_mount"
	ErrInvalidProcessSpec    Code = "invalid_process_spec"
	ErrCanceled              Code = "canceled"
	ErrUnsupportedHost       Code = "unsupported_host"
	ErrFilesystemFailed      Code = "filesystem_failed"
	ErrImageNotFound         Code = "image_not_found"
	ErrPullFailed            Code = "pull_failed"
	ErrMetadataFailed        Code = "metadata_failed"
	ErrContainerNotFound     Code = "container_not_found"
	ErrLogNotFound           Code = "log_not_found"
	ErrBundlePrepareFailed   Code = "bundle_prepare_failed"
	ErrRuntimeInstallFailed  Code = "runtime_install_failed"
	ErrRuntimeStartFailed    Code = "runtime_start_failed"
	ErrRuntimeControlFailed  Code = "runtime_control_failed"
	ErrRuntimeWaitFailed     Code = "runtime_wait_failed"
	ErrContainerExitNonzero  Code = "container_exit_nonzero"
	ErrStateConflict         Code = "state_conflict"
)
