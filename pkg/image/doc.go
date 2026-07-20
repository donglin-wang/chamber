// Package image is the public SDK entry point for Chamber image operations.
//
// The current beta surface exposes NewPuller, which returns a ready OCI image
// puller that writes OCI image layouts under a caller-provided root. Callers own
// root placement, locking, cleanup, and recovery.
package image
