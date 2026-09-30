//go:build windows

package portsmith

import "os/exec"

// setProcessGroup is a no-op on Windows. The platform has no POSIX process
// groups; Execute therefore documents the limitation that only the direct
// child is terminated, not its descendants.
func setProcessGroup(_ *exec.Cmd) {}

// killProcess terminates the direct child process. Windows lacks a portable
// process-group kill here, so descendant processes may outlive the parent.
func killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// processGroupSupported reports that process-group cancellation is not
// available on Windows.
func processGroupSupported() bool { return false }
