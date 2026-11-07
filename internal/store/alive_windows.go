//go:build windows

package store

// processAlive cannot tell on Windows: a claim of another process is taken as
// an orphan, which asks a person before sending.
func processAlive(int) bool { return false }
