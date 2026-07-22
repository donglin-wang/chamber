// Package runc provides Chamber's runc-backed runtime implementation.
//
// It downloads or reuses Chamber's pinned runc binary in the configured
// RuntimeBinDir and executes OCI runtime bundles from caller-owned storage. The
// current beta implementation supports rootless, non-terminal process isolation
// on Linux.
//
// Container.Delete delegates to runc delete for runtime state. Container.DeleteLog
// removes a selected default log stream. Callers still own bundle directories,
// image layouts, and the cached runc binary.
package runc
