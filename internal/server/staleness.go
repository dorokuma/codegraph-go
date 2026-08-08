package server

import (
	"fmt"
	"strings"
)

// maxPendingWarningFiles bounds how many pending files the staleness warning
// lists. The warning is appended AFTER the output cap in most tools, so an
// unbounded list (the watcher can hold up to 500 entries) could exceed the
// 18k payload cap and defeat truncation (audit medium).
const maxPendingWarningFiles = 50

// addStalenessWarning adds a warning about pending sync files.
func (s *Server) addStalenessWarning(text string) string {
	if w := s.Watcher.Load(); w != nil {
		pending := w.PendingFiles()
		if len(pending) > 0 {
			var warning strings.Builder
			warning.WriteString("\n\n⚠️ **Warning**: The following files have been modified but not yet synced to the index:\n")
			shown := pending
			if len(shown) > maxPendingWarningFiles {
				shown = shown[:maxPendingWarningFiles]
			}
			for _, f := range shown {
				warning.WriteString(fmt.Sprintf("- %s\n", f))
			}
			if len(pending) > len(shown) {
				fmt.Fprintf(&warning, "- … and %d more\n", len(pending)-len(shown))
			}
			warning.WriteString("\nThe index may be stale for these files. Consider reading them directly for the latest content.")
			text += warning.String()
		}
	}
	return text
}
