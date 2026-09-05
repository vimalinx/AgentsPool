//go:build windows

package worker

import (
	"os/exec"
	"time"
)

const dockerCleanupTimeout = 5 * time.Second

func configureProcess(command *exec.Cmd) {}

func terminateProcess(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	return command.Process.Kill()
}
