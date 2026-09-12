package resolution

import (
	"reflect"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// TestCallTargetKindsCrossPackageSync guards the two hand-maintained
// callTargetKinds maps (internal/db/resolve.go and
// internal/resolution/name_matcher.go) against silent drift. The packages
// cannot be merged (db is a leaf package and must stay acyclic), so the
// maps were aligned by comment only — this test turns that comment into an
// enforced contract: changing one copy without the other fails here.
func TestCallTargetKindsCrossPackageSync(t *testing.T) {
	dbKinds := db.CallTargetKinds()
	resKinds := CallTargetKinds()

	if len(resKinds) == 0 {
		t.Fatal("resolution callTargetKinds snapshot is empty")
	}
	if !reflect.DeepEqual(dbKinds, resKinds) {
		t.Fatalf("callTargetKinds drifted between internal/db and internal/resolution\n  db:         %v\n  resolution: %v\nupdate both maps (resolve.go / name_matcher.go) to stay aligned",
			dbKinds, resKinds)
	}
}
