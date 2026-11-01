//go:build windows

package store

// lockFile does nothing on Windows: the index is serialized inside one process
// only, and two servers on one folder can lose a mark there.
func lockFile(string) (func(), error) { return func() {}, nil }
