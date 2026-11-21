//go:build windows

package bridge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

func configureProcessTree(_ *exec.Cmd) {}

func terminateProcessTree(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	taskkill := filepath.Join(root, "System32", "taskkill.exe")
	killer := exec.Command(taskkill, "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F")
	_ = killer.Run()
	_ = command.Process.Kill()
}
