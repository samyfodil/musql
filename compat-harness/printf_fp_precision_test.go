package compat

// TestPrintfFPPrecisionClamp pins printf.c's SQLITE_FP_PRECISION_LIMIT
// (100000000): a %f/%e/%g conversion's precision is CLAMPED to it rather than
// erroring, unlike every other verb's precision (and unlike width, for any
// verb), which keeps the ordinary SQLITE_MAX_LENGTH "string or blob too big"
// check. %g/%e's own trailing-zero trimming keeps the rendered OUTPUT short
// regardless of how large the (clamped) precision is.
import "testing"

func TestPrintfFPPrecisionClamp(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"g-huge-prec-small-val", []string{"SELECT printf('%.*g',2147483647,0.01)"}},
		{"e-huge-prec", []string{"SELECT printf('%.*e',2147483647,1.5)"}},
		{"f-huge-prec-within-limit", []string{"SELECT printf('%.100000000f',1.5)"}},
		{"f-huge-prec-large-val", []string{"SELECT length(printf('%.*f',2147483647,123.456))"}},
		{"G-upper-huge-prec", []string{"SELECT printf('%.*G',2147483647,0.001)"}},
		{"d-huge-prec-still-errors", []string{"SELECT printf('%.*d',2147483647,5)"}},
		{"normal-precision-unaffected", []string{"SELECT printf('%.3f',3.14159), printf('%.2g',12345.6789), printf('%.5d',42)"}},
		{"c-width-still-errors", []string{"SELECT printf('%.*c', 1000000001, 'x')"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, c.stmts)
		})
	}
}
