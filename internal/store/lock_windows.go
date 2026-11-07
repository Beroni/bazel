//go:build windows

package store

import "os"

// lockFile does nothing on Windows: the index is serialized inside one process
// only, and two servers on one folder can lose a mark there.
func lockFile(string) (func(), error) { return func() {}, nil }

// tryLock does not lock on Windows: two Bazels on one folder are not kept from
// sending the same review there, the same limit as the index and the queue.
func tryLock(*os.File) bool { return true }

func unlock(*os.File) {}
