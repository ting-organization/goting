//go:build unix

package daemon

import "syscall"

// openFileLimit is the most files this process may have open. The Go
// runtime raises the soft limit to the hard one as the process starts, so
// this is as far as the system lets it go.
func openFileLimit() (uint64, bool) {
	var r syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r); err != nil {
		return 0, false
	}
	return uint64(r.Cur), true
}
