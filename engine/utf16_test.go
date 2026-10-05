package engine

import (
	"sort"
	"testing"
)

// TestUTF16CompareMatchesCSQLiteOrder verifies UTF-16 comparison semantics.
func TestUTF16CompareMatchesCSQLiteOrder(t *testing.T) {
	in := []string{"héllo", "z", "é", "\U0001F600ab"}
	for _, tc := range []struct {
		enc  TextEncoding
		want []string
	}{
		{UTF8, []string{"héllo", "z", "é", "\U0001F600ab"}},
		{UTF16LE, []string{"\U0001F600ab", "héllo", "z", "é"}},
		{UTF16BE, []string{"héllo", "z", "é", "\U0001F600ab"}},
	} {
		got := append([]string(nil), in...)
		sort.SliceStable(got, func(i, j int) bool {
			if tc.enc == UTF8 {
				return got[i] < got[j]
			}
			return utf16CompareUTF8(tc.enc, []byte(got[i]), []byte(got[j])) < 0
		})
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("enc %d: got %q want %q", tc.enc, got, tc.want)
				break
			}
		}
	}
	for _, tc := range []struct {
		enc      TextEncoding
		a, b     string
		wantLess bool
	}{
		{UTF16LE, "\U0001F600", "z", true},
		{UTF16LE, "\U0001F600", "�", true},
		{UTF16BE, "\U0001F600", "z", false},
		{UTF16BE, "\U0001F600", "�", true},
		{UTF16LE, "é", "z", false},
		{UTF16BE, "é", "z", false},
	} {
		if got := utf16CompareUTF8(tc.enc, []byte(tc.a), []byte(tc.b)) < 0; got != tc.wantLess {
			t.Errorf("enc %d: %q < %q = %v, want %v", tc.enc, tc.a, tc.b, got, tc.wantLess)
		}
	}
}

// TestUTF16RoundTripAndMemcmpAgreement verifies UTF-16 transcoding and comparison.
func TestUTF16RoundTripAndMemcmpAgreement(t *testing.T) {
	for _, enc := range []TextEncoding{UTF16LE, UTF16BE} {
		for _, s := range []string{"", "a", "héllo", "\U0001F600ab", "z", "愀"} {
			enc16 := encodeTextBytes(enc, []byte(s))
			if got := string(decodeTextBytes(enc, enc16)); got != s {
				t.Errorf("enc %d: round trip of %q gave %q", enc, s, got)
			}
			for _, o := range []string{"", "a", "héllo", "\U0001F600ab", "z", "愀"} {
				want := bytesCompare(enc16, encodeTextBytes(enc, []byte(o)))
				if got := utf16CompareUTF8(enc, []byte(s), []byte(o)); sign(got) != sign(want) {
					t.Errorf("enc %d: %q vs %q: utf16CompareUTF8=%d, memcmp of encoded=%d", enc, s, o, got, want)
				}
			}
		}
	}
	for _, enc := range []TextEncoding{UTF16LE, UTF16BE} {
		if got := decodeTextBytes(enc, []byte{0x61}); len(got) != 0 {
			t.Errorf("enc %d: odd-length blob decoded to %q, want empty", enc, got)
		}
	}
}

func bytesCompare(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
