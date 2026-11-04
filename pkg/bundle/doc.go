// Package bundle defines the public SDK contracts and configuration for Chamber
// bundle provisioning.
//
// Use the factory subpackage to construct Chamber's built-in bundle
// provisioners. The current implementation is the directory-backed rootless
// provisioner selected with ProvisionerNameDirectory.
// Provisioned bundles permit nested rootless runtimes by allowing mapping
// helpers and nested /proc mounts. Images still supply helpers and inner
// subordinate ID ranges that fit the host user's allocation.
package bundle
