//go:build vec1simd

package vec1

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/cpu"
)

// The SIMD oracle is vec1 built for speed. It sums in float32 in a different
// order than the portable code (and fuses multiply-adds), so a distance, or a
// float vec1 stores, differs from musql's in the low bits, and two rows whose
// distances differ by less than that can swap places. musql has one portable
// kernel and reproduces the scalar build to the bit (the default build tests
// that); against this build the comparison is exact in everything except:
//
//   - a column named like a distance, which may differ by tolerance, and rows
//     whose distances are within tolerance of one another, which may come in
//     either order (a run of such rows must hold the same rows);
//   - the norm vec1 stores with each binary-quantized vector, which vec1's own
//     integrity check also compares with a tolerance (1e-6) and whose bit
//     codes it requires to match exactly;
//   - the kernel name in vec1_info(): musql runs no SIMD kernel, so it says
//     "Scalar" where the oracle says "AVX2" or "NEON".

const (
	// distRel and distAbs bound a float32-accumulated distance's error against
	// the portable one: 2e-6 relative is a handful of float32 ulps over the
	// dimensions the tests use, and the absolute term covers a cosine
	// distance close to zero, which is 1 minus a float32 near 1.
	distRel = 2e-6
	distAbs = 1e-4
	// normRel is vec1's own tolerance for a stored BQ norm.
	normRel = 1e-6
)

// TestMain skips the package where the CPU cannot run the code vec1 was
// compiled for (-march=haswell), which would die with SIGILL.
func TestMain(m *testing.M) {
	if need := missingCPUFeature(); need != "" {
		fmt.Printf("SKIP: the vec1simd oracle needs %s, which this CPU lacks\n", need)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func missingCPUFeature() string {
	if runtime.GOARCH != "amd64" || (cpu.X86.HasAVX2 && cpu.X86.HasFMA && cpu.X86.HasBMI1 && cpu.X86.HasBMI2) { // arm64 builds with NEON
		return ""
	}
	return "AVX2, FMA and BMI"
}

func close32(a, b, rel, abs float64) bool {
	return math.Abs(a-b) <= rel*math.Max(math.Abs(a), math.Abs(b))+abs
}

var infoRE = regexp.MustCompile(`^'version (\S+) \((Scalar|AVX2|NEON),`)

// cellsEqual compares two cells; dist says a REAL may differ by tolerance.
func cellsEqual(a, b cell, dist bool) bool {
	if a.s == b.s {
		return true
	}
	if dist && a.isF && b.isF {
		return close32(a.f, b.f, distRel, distAbs)
	}
	// vec1_info(): only the kernel name differs.
	ma, mb := infoRE.FindStringSubmatch(a.s), infoRE.FindStringSubmatch(b.s)
	if ma != nil && mb != nil {
		return strings.Replace(a.s, "("+ma[2]+",", "(K,", 1) == strings.Replace(b.s, "("+mb[2]+",", "(K,", 1) &&
			ma[2] == "Scalar"
	}
	return false
}

func isDistCol(name string) bool { return strings.Contains(strings.ToLower(name), "distance") }

func equalResults(got, want result) bool {
	if got.err != want.err || len(got.rows) != len(want.rows) {
		return false
	}
	dist := -1 // the first distance column, which orders the rows
	isDist := make([]bool, len(want.cols))
	for i, c := range want.cols {
		if isDistCol(c) {
			isDist[i] = true
			if dist < 0 {
				dist = i
			}
		}
	}
	rowEqual := func(a, b []cell) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if !cellsEqual(a[i], b[i], isDist[i]) {
				return false
			}
		}
		return true
	}
	if dist < 0 {
		for i := range want.rows {
			if !rowEqual(got.rows[i], want.rows[i]) {
				return false
			}
		}
		return true
	}
	// Rows in order, except within a run whose distances are all within
	// tolerance of their neighbour's: there only the rows must agree.
	n := len(want.rows)
	for i := 0; i < n; {
		j := i + 1
		for j < n && wantClose(want.rows[j-1][dist], want.rows[j][dist]) {
			j++
		}
		if j == i+1 {
			if !rowEqual(got.rows[i], want.rows[i]) {
				return false
			}
		} else if !sameRows(got.rows[i:j], want.rows[i:j], isDist, rowEqual) {
			return false
		}
		i = j
	}
	return true
}

func wantClose(a, b cell) bool {
	return a.isF && b.isF && close32(a.f, b.f, distRel, distAbs)
}

// sameRows says two runs hold the same rows in some order, pairing them by
// their non-distance cells.
func sameRows(a, b [][]cell, isDist []bool, rowEqual func(a, b []cell) bool) bool {
	key := func(r []cell) string {
		var sb strings.Builder
		for i, c := range r {
			if i < len(isDist) && isDist[i] {
				continue
			}
			sb.WriteString(c.s)
			sb.WriteByte('|')
		}
		return sb.String()
	}
	ka, kb := slices.Clone(a), slices.Clone(b)
	slices.SortStableFunc(ka, func(x, y []cell) int { return strings.Compare(key(x), key(y)) })
	slices.SortStableFunc(kb, func(x, y []cell) int { return strings.Compare(key(x), key(y)) })
	for i := range ka {
		if !rowEqual(ka[i], kb[i]) {
			return false
		}
	}
	return true
}

// batchQuirk says model is one where vec1's own answer depends on how wide
// its batches are. vec1 quantizes vectors four (portable build) or eight
// (AVX2 build) at a time and the rest, fewer than a batch, one by one; the
// one-by-one path (vec1TransformInputVector) applies a model's Walsh-Hadamard
// transform but then skips the normalization a cosine distance needs, which
// the batch path (vec1TransformInputVectorN) does. So under a cosine distance
// with the transform (wht:true, or a binary quantizer, which turns it on) the
// vectors in the tail join another list, and which vectors are in the tail
// is the build's batch width. musql follows the portable build, as its
// vec1_info() says; the lists of this build are not the ones to compare with.
func batchQuirk(model string) bool {
	return strings.Contains(model, `"cos"`) &&
		(strings.Contains(model, `wht: true`) || strings.Contains(model, `"bq"`))
}

// equalMeta compares v_meta rows, which are laid out per list.
func equalMeta(got, want result, model string) bool {
	return batchQuirk(model) || equalResults(got, want)
}

// equalIdx compares v_idx rows (id, bucket, first, last, hex(val)). A list is
// a 12-byte header (flags, entry count, and a word), the rowids, then the
// codes. Where the lists depend on the batch width (batchQuirk) only the set
// of indexed rowids is compared. For binary-quantized lists, whose blocks of
// 16 vectors begin with 16 float32 norms, everything must match to the byte
// but the norms.
func equalIdx(got, want result, model string) bool {
	switch {
	case batchQuirk(model):
		return sameIndexed(got, want)
	case strings.Contains(model, `"bq"`):
		return equalBQ(got, want)
	}
	return equalResults(got, want)
}

// sameIndexed says the lists of two v_idx dumps index the same rowids.
func sameIndexed(got, want result) bool {
	ids := func(r result) ([]uint64, bool) {
		var out []uint64
		for _, row := range r.rows {
			if len(row) != 5 {
				return nil, false
			}
			b, ok := blobOf(row[4].s)
			if !ok || len(b) < 12 {
				return nil, false
			}
			n := int(binary.BigEndian.Uint32(b[4:]))
			sz := 4
			if binary.BigEndian.Uint32(b)&1 != 0 {
				sz = 8
			}
			if 12+n*sz > len(b) {
				return nil, false
			}
			for k := range n {
				if sz == 8 {
					out = append(out, binary.BigEndian.Uint64(b[12+8*k:]))
				} else {
					out = append(out, uint64(binary.BigEndian.Uint32(b[12+4*k:])))
				}
			}
		}
		slices.Sort(out)
		return out, true
	}
	g, ok1 := ids(got)
	w, ok2 := ids(want)
	return got.err == want.err && ok1 && ok2 && slices.Equal(g, w)
}

func equalBQ(got, want result) bool {
	if got.err != want.err || len(got.rows) != len(want.rows) {
		return false
	}
	for i := range want.rows {
		g, w := got.rows[i], want.rows[i]
		if len(g) != 5 || len(w) != 5 {
			return false
		}
		for k := range 4 {
			if g[k].s != w[k].s {
				return false
			}
		}
		if g[4].s == w[4].s {
			continue
		}
		gb, ok1 := blobOf(g[4].s)
		wb, ok2 := blobOf(w[4].s)
		if !ok1 || !ok2 || len(gb) != len(wb) || len(gb) < 12 {
			return false
		}
		n := int(binary.BigEndian.Uint32(wb[4:]))
		szRowid := 4
		if binary.BigEndian.Uint32(wb)&1 != 0 {
			szRowid = 8
		}
		body := len(wb) - 12 - n*szRowid
		nBlk := (n + 15) / 16
		if body < 0 || nBlk == 0 || body%nBlk != 0 || (body/nBlk)%16 != 0 {
			return false
		}
		szBlk := body / nBlk
		// Mask the norms in copies, compare the rest, then the norms.
		gm, wm := slices.Clone(gb), slices.Clone(wb)
		for blk := range nBlk {
			off := 12 + n*szRowid + blk*szBlk
			clear(gm[off : off+64])
			clear(wm[off : off+64])
		}
		if !slices.Equal(gm, wm) {
			return false
		}
		for blk := range nBlk {
			off := 12 + n*szRowid + blk*szBlk
			for s := range 16 {
				a := float64(math.Float32frombits(binary.LittleEndian.Uint32(gb[off+4*s:])))
				b := float64(math.Float32frombits(binary.LittleEndian.Uint32(wb[off+4*s:])))
				if a != b && !close32(a, b, normRel, 0) {
					return false
				}
			}
		}
	}
	return true
}

func blobOf(s string) ([]byte, bool) {
	s = strings.Trim(s, "'")
	b, err := hex.DecodeString(s)
	return b, err == nil
}
