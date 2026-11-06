//go:build windows

package server

// holdQueue has no lock on Windows: two servers on one folder share the file.
func holdQueue(string) (func(), bool) { return func() {}, true }
