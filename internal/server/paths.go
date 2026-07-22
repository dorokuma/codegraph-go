package server

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// resolvePath resolves p relative to the server's workdir.
func (s *Server) resolvePath(p string) (string, error) {
	return s.resolvePathIn(s.Workdir, p)
}

// resolvePathIn joins p under root and rejects escapes outside root.
// When Workdir is "/" the entire filesystem is the workspace — this is
// intentional for full-disk indexing scenarios and is not a sandbox escape.
func (s *Server) resolvePathIn(root, p string) (string, error) {
	if p == "" {
		return root, nil
	}
	var target string
	if filepath.IsAbs(p) {
		target = filepath.Clean(p)
	} else {
		target = filepath.Clean(filepath.Join(root, p))
	}
	// When root is "/", root+sep becomes "//" which breaks HasPrefix.
	// Direct equality check handles this edge case.
	if target == root {
		return target, nil
	}
	// Normalize root for prefix comparison: strip trailing separator before appending.
	cleanRoot := strings.TrimSuffix(root, string(filepath.Separator))
	if !strings.HasPrefix(target, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside workspace %q", p, root)
	}
	return target, nil
}

// recoverableProjectErr turns "not indexed" into a success-shaped guidance
// result (no isError) so agents don't abandon codegraph for the whole session.
func recoverableProjectErr(err error) (*mcp.CallToolResult, any, error) {
	if err == nil {
		return nil, nil, nil
	}
	msg := err.Error()
	if strings.Contains(msg, "no .codegraph index") || strings.Contains(msg, "isn't indexed") {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: msg + "\nUse built-in Read/Grep for that path this session, or pass projectPath to an indexed project."}},
		}, nil, nil
	}
	return nil, nil, err
}
