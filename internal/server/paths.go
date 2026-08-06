package server

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// resolvePath resolves p relative to the server's workdirs. It tries each
// workdir in order; the first workdir under which p resolves is returned.
func (s *Server) resolvePath(p string) (string, error) {
	for _, wd := range s.Workdirs {
		if result, err := s.resolvePathIn(wd, p); err == nil {
			return result, nil
		}
	}
	return "", fmt.Errorf("path %q is outside all workdirs %v", p, s.Workdirs)
}

// resolvePathIn joins p under root and rejects escapes outside root (B6/W8).
// Both the workspace root and the target are symlink-resolved before the jail
// check, so a symlink inside the workspace cannot smuggle access (search,
// files, explore, node …) outside it. When the target itself does not exist
// yet, the deepest existing ancestor is resolved and the missing tail
// re-appended before the check. Returns the canonical (real) path.
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
	// When root is "/", root+sep becomes "//" which breaks prefix checks.
	// Direct equality handles this edge case.
	if target == root {
		return target, nil
	}
	realRoot := s.realRoot(root)
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		// Target does not fully exist — resolve the deepest existing ancestor
		// and re-append the missing tail, so symlinks in the existing prefix
		// are still followed (and rejected if they escape).
		realTarget, err = resolveExistingAncestor(target)
		if err != nil {
			return "", fmt.Errorf("path %q is outside workspace %q", p, root)
		}
	}
	if !pathWithinRealRoot(realRoot, realTarget) {
		return "", fmt.Errorf("path %q resolves to %q outside workspace %q (symlink escape)", p, realTarget, root)
	}
	return realTarget, nil
}

// realRoot returns the canonical (EvalSymlinks-resolved) form of root,
// cached per root so resolvePathIn does not re-resolve on every call (B6).
// Falls back to the cleaned logical path when resolution fails.
func (s *Server) realRoot(root string) string {
	if s == nil {
		rp, err := filepath.EvalSymlinks(root)
		if err != nil || rp == "" {
			return filepath.Clean(root)
		}
		return rp
	}
	s.pathMu.Lock()
	if s.realRoots == nil {
		s.realRoots = map[string]string{}
	}
	if rp, ok := s.realRoots[root]; ok {
		s.pathMu.Unlock()
		return rp
	}
	s.pathMu.Unlock()
	rp, err := filepath.EvalSymlinks(root)
	if err != nil || rp == "" {
		rp = filepath.Clean(root)
	}
	s.pathMu.Lock()
	s.realRoots[root] = rp
	s.pathMu.Unlock()
	return rp
}

// resolveExistingAncestor resolves the deepest existing ancestor of path and
// re-appends the missing tail segments, producing the canonical form of a
// path that does not fully exist yet.
func resolveExistingAncestor(path string) (string, error) {
	tail := ""
	cur := path
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if tail == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, tail), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// pathWithinRealRoot reports whether target (already symlink-resolved) stays
// inside root (also resolved). Both are cleaned absolute paths; equality is
// allowed (the root itself).
func pathWithinRealRoot(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if root == "" || target == "" {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
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
