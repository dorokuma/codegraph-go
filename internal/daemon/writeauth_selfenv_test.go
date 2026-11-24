package daemon

import "testing"

// ---------- non-Linux self-env verdict (shared, platform-independent) ----------

// fakeGetenv returns a getenv closure over a fixed env map.
func fakeGetenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// TestSelfEnvWriteAuth covers the platform-independent marker verdict the
// non-Linux stub applies to the process's own environment: clean env and a
// declared worker allow; every explicit read-only marker denies.
func TestSelfEnvWriteAuth(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"clean env allows", map[string]string{"USER": "dev"}, true},
		{"CTXMODE_READONLY=1 denies", map[string]string{"CTXMODE_READONLY": "1"}, false},
		{"CTXMODE_READONLY=true denies", map[string]string{"CTXMODE_READONLY": "TrUe"}, false},
		{"CTXMODE_READONLY=0 allows", map[string]string{"CTXMODE_READONLY": "0"}, true},
		{"PI_ROLE non-empty denies", map[string]string{"PI_ROLE": "scout"}, false},
		{"CTXMODE_ROLE=scout denies", map[string]string{"CTXMODE_ROLE": "scout"}, false},
		{"CTXMODE_ROLE=oracle denies", map[string]string{"CTXMODE_ROLE": "oracle"}, false},
		{"CTXMODE_ROLE=worker allows", map[string]string{"CTXMODE_ROLE": "worker"}, true},
		{"CTXMODE_ROLE=WORKER case-insensitive", map[string]string{"CTXMODE_ROLE": "  WoRkEr "}, true},
		{"PI_SUBAGENT=1 alone denies", map[string]string{"PI_SUBAGENT": "1"}, false},
		{"PI_SUBAGENT=1 with worker allows", map[string]string{"PI_SUBAGENT": "1", "CTXMODE_ROLE": "worker"}, true},
		{"PI_SUBAGENT=1 with scout denies", map[string]string{"PI_SUBAGENT": "1", "CTXMODE_ROLE": "scout"}, false},
		{"PI_SUBAGENT=0 allows", map[string]string{"PI_SUBAGENT": "0"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := selfEnvWriteAuthFrom(fakeGetenv(c.env))
			if got.Allowed != c.want {
				t.Fatalf("selfEnvWriteAuthFrom(%v) allowed=%v want %v (reason %q)", c.env, got.Allowed, c.want, got.Reason)
			}
			if got.Allowed && got.Reason == "" || !got.Allowed && got.Reason == "" {
				t.Fatalf("verdict must always carry a reason, got %+v", got)
			}
		})
	}
}
