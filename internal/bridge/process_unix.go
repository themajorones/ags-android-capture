//go:build !windows

package bridge

import (
	"os/exec"
	"syscall"
	"time"
)

func configureProcessTree(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessTree(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	processGroup := command.Process.Pid
	_ = syscall.Kill(-processGroup, syscall.SIGTERM)
	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-processGroup, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = syscall.Kill(-processGroup, syscall.SIGKILL)
}
