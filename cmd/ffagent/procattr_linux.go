//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// setProcAttr makes the kernel kill ffmpeg if the agent dies.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
