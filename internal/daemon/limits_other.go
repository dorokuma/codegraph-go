//go:build !unix

package daemon

import "errors"

// setNice is a no-op stub on platforms without unix priorities (Windows
// plan9). ApplyProcessLimits treats the returned error as non-fatal.
func setNice(int) error {
	return errors.New("setpriority not supported on this platform")
}
