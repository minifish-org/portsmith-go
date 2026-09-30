//go:build !windows

package portsmith

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes the child a process-group leader so a cancellation or
// timeout can terminate the whole tree (upstream `detached: true`).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcess kills the child's process group, falling back to the process
// itself when the group no longer exists.
func killProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// processGroupSupported reports that process-group cancellation is available.
func processGroupSupported() bool { return true }
