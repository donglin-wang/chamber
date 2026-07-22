// Package bundle is the public SDK entry point for Chamber bundle provisioning.
//
// NewProvisioner validates the caller-provided configuration, creates private
// bundle storage, checks implementation capabilities, and returns a ready
// provisioner. The current implementation is the directory-backed rootless
// provisioner selected with shared.ProvisionerNameDirectory.
package bundle
