//go:build !windows

package server

import (
	"os"
	"syscall"
)

// holdQueue takes the lock that says which server keeps the queue file, or
// says another one has it. The system lets go of it if the process dies.
func holdQueue(path string) (release func(), ok bool) {
	// A lock that cannot be made is not held: the queue stays in memory, said
	// on the terminal and on the page, rather than shared without one.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return func() { f.Close() }, true
}
