// Package runc provides Chamber's runc-backed runtime implementation.
//
// It downloads or reuses Chamber's pinned runc binary in the configured
// RuntimeBinDir and executes OCI runtime bundles from caller-owned storage. The
// current beta implementation supports rootless process isolation on Linux.
//
// Delete delegates to runc delete for runtime state. It does not remove bundle
// directories, image layouts, runtime logs, or the cached runc binary.
package runc
