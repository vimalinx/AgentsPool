//go:build !windows

package worker

import (
	"os/exec"
	"syscall"
	"time"
)

const dockerCleanupTimeout = 5 * time.Second

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcess(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(command.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		time.Sleep(750 * time.Millisecond)
		return syscall.Kill(-pgid, syscall.SIGKILL)
	}
	return command.Process.Kill()
}
