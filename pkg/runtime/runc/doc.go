// Package runc provides Chamber's runc-backed runtime implementation.
//
// It downloads or reuses Chamber's pinned runc binary in the configured
// RuntimeBinDir and executes OCI runtime bundles from caller-owned storage. The
// current beta implementation supports rootless process isolation on Linux.
package runc
