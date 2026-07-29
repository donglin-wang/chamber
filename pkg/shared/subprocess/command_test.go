package subprocess

import (
	"syscall"
	"testing"
)

func TestCommandSetsParentDeathSignal(t *testing.T) {
	command := Command("example")
	if command.SysProcAttr == nil {
		t.Fatal("Command SysProcAttr = nil, want parent-death protection")
	}
	if command.SysProcAttr.Pdeathsig != syscall.SIGTERM {
		t.Fatalf("Command Pdeathsig = %v, want %v", command.SysProcAttr.Pdeathsig, syscall.SIGTERM)
	}
}

func TestSetProcessGroupPreservesParentDeathSignal(t *testing.T) {
	command := Command("example")

	SetProcessGroup(command)

	if command.SysProcAttr == nil {
		t.Fatal("Command SysProcAttr = nil, want process attributes")
	}
	if !command.SysProcAttr.Setpgid {
		t.Fatal("Command Setpgid = false, want true")
	}
	if command.SysProcAttr.Pdeathsig != syscall.SIGTERM {
		t.Fatalf("Command Pdeathsig = %v, want %v", command.SysProcAttr.Pdeathsig, syscall.SIGTERM)
	}
}
