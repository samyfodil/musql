package engine

import "testing"


// TestResourceLimitsEngineDirect verifies that resource limit pragmas work
// on the engine's direct execution path.
func TestResourceLimitsEngineDirect(t *testing.T) {
	db, err := Create(t.TempDir()+"/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`PRAGMA soft_heap_limit`,
		`PRAGMA soft_heap_limit=123456`,
		`PRAGMA soft_heap_limit`,
		`PRAGMA soft_heap_limit(-1)`,
		`PRAGMA soft_heap_limit`,
		`PRAGMA soft_heap_limit(0)`,
		`PRAGMA soft_heap_limit`,
		`PRAGMA threads=7`,
		`PRAGMA threads=5`,
		`PRAGMA threads`,
		`PRAGMA hard_heap_limit`,
	} {
		if err := db.Exec(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	// hard_heap_limit assignment must stay declined (process-wide constraint).
	if err := db.Exec(`PRAGMA hard_heap_limit=5000`); err == nil {
		t.Error(`PRAGMA hard_heap_limit=5000 was accepted -- see pragmaHardHeapLimit`)
	}
}
