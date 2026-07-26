// Package runc provides Chamber's runc-backed runtime implementation.
//
// It uses a configured local runtime binary or downloads Chamber's pinned runc
// binary into the configured RuntimeBinDir, then executes OCI runtime bundles
// from caller-owned storage. The current implementation supports rootless,
// non-terminal process isolation on Linux.
//
// Container.Delete delegates to runc delete for runtime state. Container.DeleteLog
// removes a selected default log stream. Callers still own bundle directories,
// image layouts, and the cached runc binary.
package runc
