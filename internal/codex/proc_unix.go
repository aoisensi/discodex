//go:build unix || linux || darwin

package codex

import (
	"os/exec"
	"syscall"
)

func setProcAttrs(cmd *exec.Cmd) {
	// Start the child in a new process group so we can kill the group
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// negative pid => process group
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
