package server

import (
	"fmt"
	"strings"
)

// addStalenessWarning adds a warning about pending sync files.
func (s *Server) addStalenessWarning(text string) string {
	if w := s.Watcher.Load(); w != nil {
		pending := w.PendingFiles()
		if len(pending) > 0 {
			var warning strings.Builder
			warning.WriteString("\n\n⚠️ **Warning**: The following files have been modified but not yet synced to the index:\n")
			for _, f := range pending {
				warning.WriteString(fmt.Sprintf("- %s\n", f))
			}
			warning.WriteString("\nThe index may be stale for these files. Consider reading them directly for the latest content.")
			text += warning.String()
		}
	}
	return text
}
