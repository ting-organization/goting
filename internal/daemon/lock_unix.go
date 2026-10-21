//go:build unix

package daemon

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on f without waiting.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
