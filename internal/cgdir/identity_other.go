//go:build !unix

package cgdir

// Non-unix fallback: there is no syscall.Stat_t to fingerprint directories
// with, so the identity degrades to the zero value and no error. Consumers
// mirror this on their side (see daemon's dirid_other.go): identity checks
// degrade to "always matches" and the cgdir symlink jail remains the guard
// line.
func statIdentity(dir string) (Identity, error) { return Identity{}, nil }
