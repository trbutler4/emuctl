//go:build !unix

package emulator

// LockStateDir is a no-op where flock is unavailable.
func LockStateDir(dir string) (func(), error) { return func() {}, nil }
