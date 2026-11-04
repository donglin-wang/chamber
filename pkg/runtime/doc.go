// Package runtime defines the public SDK contracts and configuration for Chamber
// container execution.
//
// Use the factory subpackage to construct a ready rootless runc runtime on Linux.
// RunImage provisions a local image and returns a container whose Delete method
// also removes its bundle. Run accepts caller-owned, ready OCI bundles.
// Chamber provisioning permits nested rootless runtimes by default. Run executes
// the supplied bundle without changing its spec.
package runtime
