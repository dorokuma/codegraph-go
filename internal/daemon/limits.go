package daemon

import (
	"log/slog"
	"os"
	"runtime"
)

// ApplyProcessLimits applies the configured process resource preferences to
// the current process. Both knobs are best-effort and never abort startup:
//
//   - nice > 0 lowers scheduling priority via setpriority(2). Failure is
//     logged at debug level only (e.g. insufficient privileges).
//   - maxProcs > 0 caps runtime.GOMAXPROCS, but only when the GOMAXPROCS
//     env var is not already set — an explicit env always wins.
//
// Call it once right after LoadConfig in main so client, daemon and direct
// modes all honour the settings.
func ApplyProcessLimits(nice, maxProcs int) {
	if nice > 0 {
		if err := setNice(nice); err != nil {
			slog.Debug("setpriority failed, continuing without nice", "nice", nice, "error", err)
		}
	}
	if maxProcs > 0 && os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(maxProcs)
	}
}
