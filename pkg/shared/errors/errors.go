package errors

// Code is Chamber's stable, JSON-friendly error vocabulary. It also satisfies
// error so callers can use errors.Is with wrapped operation failures.
type Code string

func (c Code) Error() string {
	return string(c)
}

const (
	ErrInvalidRequest       Code = "invalid_request"
	ErrImageNotFound        Code = "image_not_found"
	ErrPullFailed           Code = "pull_failed"
	ErrMetadataFailed       Code = "metadata_failed"
	ErrContainerNotFound    Code = "container_not_found"
	ErrLogNotFound          Code = "log_not_found"
	ErrBundlePrepareFailed  Code = "bundle_prepare_failed"
	ErrRuntimeStartFailed   Code = "runtime_start_failed"
	ErrRuntimeWaitFailed    Code = "runtime_wait_failed"
	ErrContainerExitNonzero Code = "container_exit_nonzero"
	ErrStateConflict        Code = "state_conflict"
)
