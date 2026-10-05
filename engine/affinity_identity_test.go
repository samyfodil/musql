package engine

import (
	"reflect"
	"testing"
)

// Every (type, affinity) pair affinityIsIdentity calls an identity must leave
// every value of that type unchanged under the full rule -- OpAffinity and the
// comparison paths skip the rule on its word.
func TestAffinityIdentityClaimsHold(t *testing.T) {
	vals := []Value{
		{Typ: Null},
		{Typ: Int, I: 5}, {Typ: Int, I: -1 << 62},
		{Typ: Float, F: 1.5}, {Typ: Float, F: 3},
		{Typ: Text, S: []byte("12")}, {Typ: Text, S: []byte(" 1.5 ")}, {Typ: Text, S: []byte("x")},
		{Typ: Blob, S: []byte("12")},
	}
	for _, aff := range []affinity{affNone, affText, affNumeric, affInteger, affReal} {
		for _, v := range vals {
			if affinityIsIdentity(v.Typ, aff) {
				if got := applyAffinitySlow(v, aff); !reflect.DeepEqual(got, v) {
					t.Errorf("affinity %d claims identity for %v, but the rule gives %v", aff, v, got)
				}
			}
		}
	}
}
