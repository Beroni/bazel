//go:build !windows

package store

import (
	"errors"
	"syscall"
)

// processAlive says whether pid is a running process.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
