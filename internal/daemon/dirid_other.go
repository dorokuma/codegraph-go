//go:build !unix

package daemon

// Non-unix fallback: there is no syscall.Stat_t to fingerprint directories
// with, so identity verification degrades to "always matches" and every
// removal behaves exactly as before this change. The .codegraph symlink
// jail (cgdir.Ensure, run by Start and db.Open) remains the guard line.

type dirIdentity struct{}

func statDirIdentity(dir string) (*dirIdentity, error) { return &dirIdentity{}, nil }

func (id *dirIdentity) matches(dir string) bool { return true }
