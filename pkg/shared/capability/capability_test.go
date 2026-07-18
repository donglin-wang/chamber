package capability

import "testing"

func TestPrivilegeStringValues(t *testing.T) {
	if Rootless != "rootless" {
		t.Fatalf("Rootless = %q, want rootless", Rootless)
	}
	if Rootful != "rootful" {
		t.Fatalf("Rootful = %q, want rootful", Rootful)
	}
}
