//go:build !unix

package daemon

// openFileLimit is unknown here.
func openFileLimit() (uint64, bool) { return 0, false }
