package agent

import "os/exec"

// Windows has no process groups to speak of here: only the agent itself dies.
func ownProcessGroup(*exec.Cmd) {}

func killProcessGroup(*exec.Cmd) {}
