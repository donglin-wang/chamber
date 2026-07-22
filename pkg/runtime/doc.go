// Package runtime is the public SDK entry point for Chamber runtime execution.
//
// NewRuntime validates the caller-provided configuration, checks host and
// implementation support, creates private runtime storage, and returns a ready
// runtime. The current implementation supports rootless runc on Linux.
package runtime
