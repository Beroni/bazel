//go:build !windows

package store

import (
	"os"
	"syscall"
)

// lockFile holds an exclusive lock on path across processes until the returned
// function runs. The system lets go of it if the process dies.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}
