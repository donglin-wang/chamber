// Package capability defines shared vocabulary for Chamber SDK implementation
// support declarations.
package capability

// Privilege identifies a host privilege mode supported or requested by a
// Chamber SDK component.
type Privilege string

const (
	// Rootless runs without requiring sudo or a root-owned daemon.
	Rootless Privilege = "rootless"

	// Rootful runs with root privileges and expands the local trust boundary.
	Rootful Privilege = "rootful"
)
