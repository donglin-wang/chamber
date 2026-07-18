// Package capability defines shared vocabulary for Chamber SDK implementation
// support declarations.
package capability

type Privilege string

const (
	Rootless Privilege = "rootless"
	Rootful  Privilege = "rootful"
)
