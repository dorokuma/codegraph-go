package extraction

import (
	"strings"

	"github.com/dorokuma/codegraph-go/db"
)

// noisyRefNames are call/reference targets that are almost always framework /
// host plumbing, not project symbols. Used ONLY to scrub failed unresolved_refs
// AFTER resolve when nothing in the index matches the name.
//
// Rules:
//   - Never drop at promote (same-file link needs the ref).
//   - Never drop at park if a project node with that name exists.
//   - Never list real domain names (add/new/close/remove/delete/handler/…).
//
// Synthesis still scans source text for emit/on/setState patterns.
var noisyRefNames = map[string]struct{}{
	// React / DOM timers & events
	"setState": {}, "forceUpdate": {},
	"addEventListener": {}, "removeEventListener": {},
	"preventDefault": {}, "stopPropagation": {}, "stopImmediatePropagation": {},
	"setTimeout": {}, "setInterval": {}, "clearTimeout": {}, "clearInterval": {},
	"requestAnimationFrame": {}, "cancelAnimationFrame": {},
	// EventEmitter API surface (synth reads source, not these refs)
	"emit": {}, "on": {}, "off": {}, "once": {},
	"addListener": {}, "removeListener": {},
	// tiny anonymous callback parameter names
	"cb": {}, "callback": {},
	// console methods
	"log": {}, "warn": {}, "debug": {}, "info": {}, "trace": {},
	// Promise plumbing
	"then": {}, "catch": {}, "finally": {},
	// Vue compiler macros
	"defineProps": {}, "defineEmits": {}, "defineExpose": {},
	"defineOptions": {}, "defineSlots": {}, "defineModel": {}, "withDefaults": {},
	// Svelte 5 runes
	"$props": {}, "$state": {}, "$derived": {}, "$effect": {},
	"$bindable": {}, "$inspect": {}, "$host": {}, "$snippet": {},
}

// IsNoisyRefName reports whether name looks like framework/host plumbing.
func IsNoisyRefName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	if _, ok := noisyRefNames[name]; ok {
		return true
	}
	if i := strings.LastIndexAny(name, ".#"); i >= 0 && i+1 < len(name) {
		if _, ok := noisyRefNames[name[i+1:]]; ok {
			return true
		}
	}
	return false
}

// ShouldParkRef decides whether a cross-file unknown should be stored.
// Noisy names are still parked when a project symbol with that name already
// exists (so later resolve / retries can link). Pure noise with no symbol is
// skipped to keep failed-ref tables clean.
func ShouldParkRef(database *db.DB, refName string) bool {
	if refName == "" {
		return false
	}
	if !IsNoisyRefName(refName) {
		return true
	}
	if database == nil {
		return false
	}
	if hasProjectSymbol(database, refName) {
		return true
	}
	return false
}

func hasProjectSymbol(database *db.DB, refName string) bool {
	names := []string{refName}
	if tail := NameTail(refName); tail != "" && tail != refName {
		names = append(names, tail)
	}
	for _, n := range names {
		nodes, err := database.GetNodeByName(n)
		if err == nil && len(nodes) > 0 {
			return true
		}
	}
	return false
}

// ScrubNoisyFailedRefs deletes failed unresolved rows that are pure noise and
// still have no matching project symbol. Call after ResolveAll / ResolveForFiles.
func ScrubNoisyFailedRefs(database *db.DB) (removed int, err error) {
	if database == nil {
		return 0, nil
	}
	failed, err := database.ListUnresolvedRefs("", "failed")
	if err != nil {
		return 0, err
	}
	for _, r := range failed {
		if !IsNoisyRefName(r.ReferenceName) {
			continue
		}
		if hasProjectSymbol(database, r.ReferenceName) {
			continue // real symbol — leave for retry
		}
		if err := database.DeleteUnresolvedRef(r.ID); err != nil {
			return removed, err
		}
		removed++
	}
	// Also drop pending pure-noise with no symbol (stale parks from older builds).
	pending, err := database.ListUnresolvedRefs("", "pending")
	if err != nil {
		return removed, err
	}
	for _, r := range pending {
		if !IsNoisyRefName(r.ReferenceName) {
			continue
		}
		if hasProjectSymbol(database, r.ReferenceName) {
			continue
		}
		if err := database.DeleteUnresolvedRef(r.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// ScrubNoisyFailedRefsForFiles is like ScrubNoisyFailedRefs but only processes
// refs belonging to the given files. When files is nil, processes all files.
// hasProjectSymbol results are cached per-call to avoid repeated DB lookups.
func ScrubNoisyFailedRefsForFiles(database *db.DB, files []string) (removed int, err error) {
	if database == nil {
		return 0, nil
	}

	// Cache hasProjectSymbol results per ref name within this call.
	cache := make(map[string]bool)
	hasSymbolCached := func(name string) bool {
		if v, ok := cache[name]; ok {
			return v
		}
		v := hasProjectSymbol(database, name)
		cache[name] = v
		return v
	}

	scrubList := func(status string) (int, error) {
		var refs []db.UnresolvedRef
		if files == nil {
			var err error
			refs, err = database.ListUnresolvedRefs("", status)
			if err != nil {
				return 0, err
			}
		} else {
			var err error
			refs, err = database.ListUnresolvedRefsByFiles(files, status)
			if err != nil {
				return 0, err
			}
		}
		count := 0
		for _, r := range refs {
			if !IsNoisyRefName(r.ReferenceName) {
				continue
			}
			if hasSymbolCached(r.ReferenceName) {
				continue
			}
			if derr := database.DeleteUnresolvedRef(r.ID); derr != nil {
				return count, derr
			}
			count++
		}
		return count, nil
	}

	n1, err := scrubList("failed")
	if err != nil {
		return n1, err
	}
	n2, err := scrubList("pending")
	return n1 + n2, err
}
