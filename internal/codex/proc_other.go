//go:build !unix && !linux && !darwin

package codex

import "os/exec"

func setProcAttrs(cmd *exec.Cmd)     {}
func killProcessGroup(cmd *exec.Cmd) {}
