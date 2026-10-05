package engine

import "testing"

// TestStoredBodyPagerHidesOriginatingReaders verifies that stored trigger
// bodies don't see the originating database in a delegated cross-database write.
func TestStoredBodyPagerHidesOriginatingReaders(t *testing.T) {
	newDB := func(t *testing.T) *Session {
		t.Helper()
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE t(a)"); err != nil {
			t.Fatal(err)
		}
		return db
	}

	t.Run("delegated session: originating readers are hidden", func(t *testing.T) {
		db := newDB(t)
		defer db.Discard()
		// Delegated session with originating readers
		origin, oerr := db.SnapshotPager()
		if oerr != nil {
			t.Fatal(oerr)
		}
		db.attachedReaders = []attachedReader{{pager: origin}}
		db.originReadersBefore = 1

		p, err := db.storedBodyPager()
		if err != nil {
			t.Fatal(err)
		}
		if len(p.attachedReaders) != 0 || p.originReadersBefore != 0 {
			t.Errorf("storedBodyPager kept %d attached reader(s) (originReadersBefore=%d); a stored "+
				"trigger body must not see the ORIGINATING database of a delegated write",
				len(p.attachedReaders), p.originReadersBefore)
		}
		// Write-subquery pager keeps the readers
		w, werr := db.writeSubqueryPager()
		if werr != nil {
			t.Fatal(werr)
		}
		if len(w.attachedReaders) != 1 {
			t.Errorf("writeSubqueryPager dropped the delegated session's readers (%d); only a "+
				"stored BODY hides them", len(w.attachedReaders))
		}
	})

	t.Run("ordinary session: its own attached readers survive", func(t *testing.T) {
		db := newDB(t)
		defer db.Discard()
		// Ordinary ATTACH with own attached readers
		other, oerr := db.SnapshotPager()
		if oerr != nil {
			t.Fatal(oerr)
		}
		db.attachedReaders = []attachedReader{{pager: other}}
		db.originReadersBefore = 0

		p, err := db.storedBodyPager()
		if err != nil {
			t.Fatal(err)
		}
		if len(p.attachedReaders) != 1 {
			t.Errorf("storedBodyPager hid an ORDINARY session's own attached reader (%d left); only "+
				"a DELEGATED session's originating readers are hidden", len(p.attachedReaders))
		}
	})
}
