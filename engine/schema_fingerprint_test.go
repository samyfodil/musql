package engine

import "testing"

// TestSchemaRowsFingerprintSeesEveryField: the plan, catalog-segment and
// read-source caches trust an equal fingerprint as the same schema, so a change
// to any field, or the same bytes split differently between fields, must move it.
func TestSchemaRowsFingerprintSeesEveryField(t *testing.T) {
	base := SchemaRow{Type: "table", Name: "ab", TblName: "c", SQL: "CREATE TABLE ab(x)", AliasOf: "", RootPage: 2, Rowid: 1}
	fp := schemaRowsFingerprint([]SchemaRow{base})
	for name, edit := range map[string]func(*SchemaRow){
		"type":     func(r *SchemaRow) { r.Type = "index" },
		"name":     func(r *SchemaRow) { r.Name = "ac" },
		"tbl_name": func(r *SchemaRow) { r.TblName = "d" },
		"sql":      func(r *SchemaRow) { r.SQL = "CREATE TABLE ab(y)" },
		"alias":    func(r *SchemaRow) { r.AliasOf = "z" },
		"rootpage": func(r *SchemaRow) { r.RootPage = 3 },
		"rowid":    func(r *SchemaRow) { r.Rowid = 2 },
		"temp":     func(r *SchemaRow) { r.Temp = true },
		"split":    func(r *SchemaRow) { r.Name, r.TblName = "a", "bc" },
	} {
		r := base
		edit(&r)
		if got := schemaRowsFingerprint([]SchemaRow{r}); got == fp || got == 0 {
			t.Errorf("%s: fingerprint %x, unedited %x", name, got, fp)
		}
	}
	if schemaRowsFingerprint([]SchemaRow{base}) != fp {
		t.Fatal("fingerprint is not stable within the process")
	}
}
