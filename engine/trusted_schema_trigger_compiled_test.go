package engine

import (
	"strings"
	"testing"
)

// TestTrustedSchemaEnforcedOnCompiledTriggerFire verifies that PRAGMA trusted_schema
// enforcement applies to unsafe functions in compiled trigger bodies.
func TestTrustedSchemaEnforcedOnCompiledTriggerFire(t *testing.T) {
	setup := []string{
		"CREATE TABLE t(a)",
		"CREATE TABLE log(m)",
		"CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(snippet(new.a)); END",
	}
	for _, tc := range []struct {
		name       string
		trusted    bool
		wantUnsafe bool
	}{
		{"trusted_schema=OFF rejects the unsafe body", false, true},
		{"trusted_schema=ON does not raise the unsafe error", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Discard()
			for _, s := range setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			db.SetTrustedSchema(tc.trusted)
			err = db.Exec("INSERT INTO t VALUES(1)")
			got := err != nil && strings.Contains(err.Error(), "unsafe use of snippet()")
			if got != tc.wantUnsafe {
				t.Errorf("INSERT fired the trigger with trusted_schema=%v: err=%v\n"+
					"  raised \"unsafe use of snippet()\"=%v, want %v", tc.trusted, err, got, tc.wantUnsafe)
			}
		})
	}
}
