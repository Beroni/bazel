//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup makes the agent the leader of a new process group, so that
// what it starts can be killed with it.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills whatever is left of the group cmd leads. A process
// that left the group on purpose, with setsid, is out of reach.
func killProcessGroup(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
