//go:build unix

package emulator

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockStateDir takes an exclusive lock on dir for this process. A second
// `emuctl serve` on the same state would fight over the same AVDs, and its
// stale-emulator cleanup would stop the first one's live emulators.
func LockStateDir(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "serve.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another emuctl serve is using %s", dir)
	}
	return func() { f.Close() }, nil
}
