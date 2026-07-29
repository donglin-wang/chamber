// Package subprocess applies Chamber's safety defaults to spawned processes.
package subprocess

import (
	"context"
	"os/exec"
	"syscall"
)

// Command returns an exec.Cmd with Chamber's subprocess safety defaults.
func Command(name string, arg ...string) *exec.Cmd {
	command := exec.Command(name, arg...)
	protectParentDeath(command)
	return command
}

// CommandContext returns an exec.Cmd with Chamber's subprocess safety defaults.
func CommandContext(ctx context.Context, name string, arg ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, name, arg...)
	protectParentDeath(command)
	return command
}

func protectParentDeath(command *exec.Cmd) {
	sysProcAttr(command).Pdeathsig = syscall.SIGTERM
}

// SetProcessGroup starts the command in a new process group.
func SetProcessGroup(command *exec.Cmd) {
	sysProcAttr(command).Setpgid = true
}

func sysProcAttr(command *exec.Cmd) *syscall.SysProcAttr {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	return command.SysProcAttr
}
