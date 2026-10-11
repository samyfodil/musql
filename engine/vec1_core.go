// This file is the numeric core of vec1, SQLite's vector extension
// (https://sqlite.org/vec1): the model blob vec1_train() writes and a vec1
// table stores, and every distance, transform and encoding computed from it.
//
// Each function is a port of vec1.c's portable C -- the code a build without
// AVX2 or NEON runs -- and gives its answer to the bit. vec1's SIMD builds sum
// in another order (and fuse multiply-adds), so their low bits move with the
// platform; the portable C is the only answer every platform shares. That is
// why a product below is always wrapped in an explicit conversion: Go may fuse
// x*y+z into one instruction on arm64 unless a conversion rounds the product
// first, and C (built without contraction) never does.
//
// The model blob, as vec1TrainFinal writes it: a 24-byte header of six
// big-endian u32s (version, flags, nElem, nCodebook, nBucket, distance), then
// the PQ codebooks, the coarse centroids and the OPQ rotation, each an array
// of float32 in machine byte order and present only when the header says so.
package engine

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

const (
	vec1DistL2  = 1
	vec1DistCos = 2

	vec1Version          = "0.7"
	vec1HeaderVersion    = 4 // VEC1_HEADER_VERSION
	vec1HeaderVersionBin = 7 // VEC1_HEADER_VERSION_BIN, for a binary quantizer
	vec1FmtVersion       = 4 // VEC1_CURRENT_FMTVERSION, %_config's id 5

	vec1HeaderSize = 24

	vec1ModelIndex    = 0x01
	vec1ModelRotate   = 0x02
	vec1ModelResidual = 0x04
	vec1ModelBitQuant = 0x08
	vec1ModelWHT      = 0x10

	vec1CodebookSize = 256 // codes per PQ codebook
	vec1PQBlock      = 16  // codes grouped together in an %_idx list

	vec1VecSizeMin = 2
	vec1VecSizeMax = 4096
	vec1MaxBucket  = 65536
	vec1MaxK       = 32 * 1024 * 1024
	vec1MaxThread  = 128

	vec1BlocksizeMin        = 1024
	vec1BlocksizeMax        = 16 * 1024 * 1024
	vec1BlocksizeDefault    = 1024 * 1024
	vec1BlocksizeMinDefault = 16 * 1024

	vec1DefaultNProbe = 0.05

	// The scalar build's multi-match widths (VEC1_MULTIMATCH_NVEC and _NCENT):
	// a rebuild quantizes four vectors at a time and the rest one by one,
	// which use different arithmetic, so the split is part of the answer.
	vec1MultiNVec = 4
	// VEC1_QUANTIZE_JOB_SZ: a rebuild's vectors are quantized in jobs of this
	// many, and each job's last few go one at a time.
	vec1QuantizeJob = 1024
)

// vec1Native is the byte order vec1 stores floats in: the machine's own.
var vec1Native = binary.NativeEndian

// vec1Floats reads a blob of native-order float32s.
func vec1Floats(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	if len(out) == 0 {
		return out
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&out[0])), 4*len(out)), b)
	return out
}

// vec1FloatBytes is the blob holding v.
func vec1FloatBytes(v []float32) []byte {
	if len(v) == 0 {
		return []byte{}
	}
	return slices.Clone(unsafe.Slice((*byte)(unsafe.Pointer(&v[0])), 4*len(v)))
}

// vec1NonFinite reports whether some element is NaN or infinite
// (vec1VectorIsNonFinite: every exponent bit set).
func vec1NonFinite(b []byte) bool {
	for i := 0; i+4 <= len(b); i += 4 {
		if vec1Native.Uint32(b[i:])&0x7F800000 == 0x7F800000 {
			return true
		}
	}
	return false
}

// vec1Dot is vec1DotProduct: a float32 sum, in component order.
func vec1Dot(a, b []float32) float32 {
	var sum float32
	for j := range a {
		sum += float32(a[j] * b[j])
	}
	return sum
}

// vec1L2 is vec1L2Dist: the squared Euclidean distance, each difference in
// float32 and its square summed in float64.
func vec1L2(a, b []float32) float64 {
	var ret float64
	for i := range a {
		d := float64(a[i] - b[i])
		ret += float64(d * d)
	}
	return ret
}

// vec1Cos is vec1CosDist: one minus the cosine, from float32 sums, with a
// zero vector taken as orthogonal to everything.
func vec1Cos(a, b []float32) float64 {
	var dot, as, bs float32
	for i := range a {
		dot += float32(a[i] * b[i])
		as += float32(a[i] * a[i])
		bs += float32(b[i] * b[i])
	}
	return vec1CosFinish(dot, as, bs)
}

// vec1CosFinish is vec1CosDist's distance from its three sums.
func vec1CosFinish(dot, as, bs float32) float64 {
	if as == 0 || bs == 0 {
		return 1.0
	}
	return 1.0 - float64(dot)/math.Sqrt(float64(float32(as*bs)))
}

// vec1Normalize is vec1NormalizeVector, in place.
func vec1Normalize(v []float32) {
	sum := float64(vec1Dot(v, v))
	if sum == 0 {
		return
	}
	sum = 1.0 / math.Sqrt(sum)
	for i := range v {
		v[i] = float32(float64(v[i]) * sum)
	}
}

// vec1WHTPow2 is vec1WHT over a power-of-two length.
func vec1WHTPow2(x []float32) {
	n := len(x)
	if n >= 16 {
		if k := vec1WHTKernel(); k != nil {
			k.Call(&jit.Args{A: unsafe.Pointer(unsafe.SliceData(x)), N: int64(n)})
			return
		}
	}
	for stride := 1; stride < n; stride <<= 1 {
		for i := 0; i < n; i += stride << 1 {
			lo, hi := x[i:i+stride], x[i+stride:i+2*stride]
			hi = hi[:len(lo)]
			for j, a := range lo {
				b := hi[j]
				lo[j], hi[j] = a+b, a-b
			}
		}
	}
}

// vec1ApplyWHT is vec1ApplyWHT: a power-of-two length transformed whole,
// any other as two overlapping power-of-two halves.
func vec1ApplyWHT(x []float32) {
	n := len(x)
	if n > 0 && n&(n-1) == 0 {
		vec1WHTPow2(x)
		return
	}
	p := 1
	for p<<1 < n {
		p <<= 1
	}
	vec1WHTPow2(x[:p])
	vec1WHTPow2(x[n-p:])
}

// vec1Sub is vec1Sub: out = a - b.
func vec1Sub(out, a, b []float32) {
	for i := range out {
		out[i] = a[i] - b[i]
	}
}

// ---- the model -------------------------------------------------------------

// vec1Header is Vec1ModelHeader.
type vec1Header struct {
	version, flags, nElem, nCodebook, nBucket, distance uint32
}

func (h *vec1Header) write(b []byte) {
	ver := uint32(vec1HeaderVersion)
	if h.flags&vec1ModelBitQuant != 0 {
		ver = vec1HeaderVersionBin
	}
	for i, v := range []uint32{ver, h.flags, h.nElem, h.nCodebook, h.nBucket, h.distance} {
		binary.BigEndian.PutUint32(b[4*i:], v)
	}
}

func vec1ReadHeader(b []byte) vec1Header {
	u := func(i int) uint32 { return binary.BigEndian.Uint32(b[4*i:]) }
	return vec1Header{u(0), u(1), u(2), u(3), u(4), u(5)}
}

// vec1Model is a decoded model, vec1DecodeModel's Vec1Model. A table with no
// model has the zero value with distance L2 (vec1CreateConnect).
type vec1Model struct {
	hdr       vec1Header
	nCodeElem int
	book      []float32 // PQ codebooks: nCodebook x 256 x nCodeElem
	cent      []float32 // coarse centroids: nBucket x nElem
	rot       []float32 // OPQ rotation: nElem x nElem, row-major
	blob      []byte    // the stored blob, which a rebuild's model check rereads
	t8m       vec1T8Model
}

func (m *vec1Model) bitQuant() bool { return m.hdr.flags&vec1ModelBitQuant != 0 }
func (m *vec1Model) indexed() bool  { return m.hdr.flags&vec1ModelIndex != 0 }
func (m *vec1Model) residual() bool { return m.hdr.flags&vec1ModelResidual != 0 }
func (m *vec1Model) cos() bool      { return m.hdr.distance == vec1DistCos }
func (m *vec1Model) isFlat() bool   { return m.indexed() && m.hdr.nCodebook == 0 }
func (m *vec1Model) nElem() int     { return int(m.hdr.nElem) }
func (m *vec1Model) nBucket() int   { return int(m.hdr.nBucket) }
func (m *vec1Model) nCodebook() int { return int(m.hdr.nCodebook) }
func (m *vec1Model) centroid(i int) []float32 {
	n := m.nElem()
	return m.cent[i*n : i*n+n]
}

// encodedSize is vec1EncodedVectorSize: a PQ code is a byte per codebook; a
// binary code adds its vector's float32 norm.
func (m *vec1Model) encodedSize() int {
	n := m.nCodebook()
	if m.bitQuant() {
		n += 4
	}
	return n
}

// transformRequired is vec1TransformRequired.
func (m *vec1Model) transformRequired() bool {
	return m.hdr.flags&(vec1ModelRotate|vec1ModelWHT) != 0 || m.cos()
}

// vec1DecodeModel is vec1DecodeModel, with its messages.
func vec1DecodeModel(blob []byte) (*vec1Model, error) {
	if len(blob) < vec1HeaderSize {
		return nil, fmt.Errorf("vec1: model too small - %d bytes", len(blob))
	}
	m := &vec1Model{hdr: vec1ReadHeader(blob), blob: blob}
	h := &m.hdr
	if h.version != vec1HeaderVersion && h.version != vec1HeaderVersionBin {
		if h.version < vec1HeaderVersionBin {
			return nil, fmt.Errorf("vec1: unrecognized model version")
		}
		return nil, fmt.Errorf("vec1: unsupported model version - require vec1 %d.%d", h.version/1000, h.version%1000)
	}
	if h.nElem > vec1VecSizeMax || h.nBucket > vec1MaxBucket || h.nCodebook > h.nElem {
		return nil, fmt.Errorf("vec1: model is corrupt")
	}
	off := vec1HeaderSize
	nElem, nCodebook := int(h.nElem), int(h.nCodebook)
	var bookN int
	if nCodebook > 0 {
		if h.flags&vec1ModelBitQuant == 0 {
			m.nCodeElem = (nElem + nCodebook - 1) / nCodebook
			bookN = nCodebook * m.nCodeElem * vec1CodebookSize
		} else {
			switch {
			case (nElem+7)/8 == nCodebook:
				m.nCodeElem = 8
			case (nElem+15)/16 == nCodebook:
				m.nCodeElem = 16
			default:
				return nil, fmt.Errorf("vec1: unrecognized bq codesize")
			}
		}
	}
	centN := 0
	if h.nBucket > 0 {
		centN = nElem * int(h.nBucket)
	}
	rotN := 0
	if h.flags&vec1ModelRotate != 0 {
		rotN = nElem * nElem
	}
	want := off + 4*(bookN+centN+rotN)
	if want != len(blob) {
		return nil, fmt.Errorf("vec1: model size mismatch: have %d, expected %d", len(blob), want)
	}
	m.book = vec1Floats(blob[off : off+4*bookN])
	off += 4 * bookN
	m.cent = vec1Floats(blob[off : off+4*centN])
	off += 4 * centN
	if rotN > 0 {
		m.rot = vec1Floats(blob[off : off+4*rotN])
	}
	return m, nil
}

// tmpLen is the length of a transformed vector's buffer: nElem, or the PQ
// codebooks' coverage when that is longer, the tail left zero
// (vec1LoadConfig's nReq).
func (m *vec1Model) tmpLen(nElem int) int {
	return max(nElem, int(m.hdr.nElem), m.nCodeElem*m.nCodebook())
}

// transform is vec1TransformInputVector: the vector the model compares, in
// tmp (len tmpLen, zero past nElem) when it differs from in, which is then
// returned. A rotation is followed by the cosine's normalization; a
// Walsh-Hadamard transform is not -- vec1.c's single-vector path differs from
// the batched one there (vec1TransformInputVectorN), and both are kept.
func (m *vec1Model) transform(tmp, in []float32) []float32 {
	n := m.nElem()
	switch {
	case m.hdr.flags&vec1ModelRotate != 0:
		m.rotate(in, tmp[:n])
		if m.cos() {
			vec1Normalize(tmp[:n])
		}
		return tmp
	case m.hdr.flags&vec1ModelWHT != 0:
		copy(tmp, in[:n])
		vec1ApplyWHT(tmp[:n])
		return tmp
	case m.cos():
		copy(tmp, in[:n])
		vec1Normalize(tmp[:n])
		return tmp
	}
	if m.nCodebook() > 0 && m.nCodebook()*m.nCodeElem != n {
		copy(tmp, in[:n])
		return tmp
	}
	return in
}

// transformN is the transform of vec1TransformInputVectorN, used by a
// rebuild's batches: the cosine's normalization follows every transform.
func (m *vec1Model) transformN(tmp, in []float32) []float32 {
	n := m.nElem()
	out := in
	switch {
	case m.hdr.flags&vec1ModelRotate != 0:
		m.rotate(in, tmp[:n])
		out = tmp
	case m.hdr.flags&vec1ModelWHT != 0:
		copy(tmp, in[:n])
		vec1ApplyWHT(tmp[:n])
		out = tmp
	case m.cos() || len(tmp) != n:
		copy(tmp, in[:n])
		out = tmp
	}
	if m.cos() {
		vec1Normalize(tmp[:n])
	}
	return out
}

// vec1BestMatch is vec1PqBestMatch: the first of nCode vectors nearest a,
// by the float64 distance. total, when not nil, gains the best distance.
func vec1BestMatch(book []float32, nCode int, a []float32, total *float64) int {
	n := len(a)
	best, bestD := 0, math.Inf(1)
	for i := range nCode {
		d := vec1L2(a, book[i*n:i*n+n])
		if d < bestD {
			best, bestD = i, d
		}
	}
	if total != nil {
		*total += bestD
	}
	return best
}

// encodePQ is vec1EncodeVectorPQ: each sub-vector's code by the float32 sums
// the query LUT uses (vec1ModelTDist), the first of a tie winning.
func (m *vec1Model) encodePQ(a []float32, code []byte) {
	ne := m.nCodeElem
	books := m.t8().book
	buf := vec1Bufs.Get().(*vec1Buf)
	for b := range m.nCodebook() {
		var k int
		k, buf.f = vec1BestT8F(books[b], vec1CodebookSize, ne, a[b*ne:b*ne+ne], buf.f)
		if k >= 0 {
			code[b] = byte(k)
		}
	}
	vec1Bufs.Put(buf)
}

// encodePQN is a rebuild's PQ code (vec1EncodeVectorN): each sub-vector's
// best match by float64 distances rounded to float32.
func (m *vec1Model) encodePQN(a []float32, code []byte) {
	ne := m.nCodeElem
	books := m.t8().book
	buf := vec1Bufs.Get().(*vec1Buf)
	for b := range m.nCodebook() {
		var k int
		k, _, buf.d = vec1BestT8(books[b], vec1CodebookSize, ne, a[b*ne:b*ne+ne], true, buf.d)
		code[b] = byte(k)
	}
	vec1Bufs.Put(buf)
}

// encodeBit is vec1EncodeVectorBit: the float32 norm, then a bit per
// dimension (or per pair) set when it is positive. a must cover
// nCodebook*nCodeElem components.
func (m *vec1Model) encodeBit(a []float32, code []byte) {
	n := m.nElem()
	norm := vec1Dot(a[:n], a[:n])
	norm = float32(math.Sqrt(float64(norm)))
	vec1Native.PutUint32(code, math.Float32bits(norm))
	out := 4
	for i := 0; i < n; i += m.nCodeElem {
		var v byte
		for j := range 8 {
			x := a[i+j]
			if m.nCodeElem != 8 {
				x = a[i+2*j] + a[i+2*j+1]
			}
			v |= vec1Positive(x) << j
		}
		code[out] = v
		out++
	}
}

// vec1Positive is 1 when x > 0, else 0, in a form the compiler makes a
// conditional move: the signs a binary code records are random, and a
// branch on each mispredicts half the time.
func vec1Positive(x float32) byte {
	var b byte
	if x > 0 {
		b = 1
	}
	return b
}

// encode is vec1EncodeVector.
func (m *vec1Model) encode(a []float32, code []byte) {
	if m.bitQuant() {
		m.encodeBit(a, code)
	} else {
		m.encodePQ(a, code)
	}
}

// decode is vec1DecodeVector: the vector a code stands for.
func (m *vec1Model) decode(code []byte, out []float32) {
	n := m.nElem()
	if m.bitQuant() {
		norm := math.Float32frombits(vec1Native.Uint32(code))
		v := float32(math.Sqrt(float64(float32(norm*norm) / float32(n))))
		for i := range n {
			var set bool
			if m.nCodeElem == 8 {
				set = code[4+i/8]&(1<<(i%8)) != 0
			} else {
				set = code[4+i/16]&(1<<((i/2)%8)) != 0
			}
			if set {
				out[i] = v
			} else {
				out[i] = -1 * v
			}
		}
		return
	}
	ne := m.nCodeElem
	for b := range m.nCodebook() {
		src := m.book[(b*vec1CodebookSize+int(code[b]))*ne:]
		cnt := min(ne, n-b*ne)
		copy(out[b*ne:b*ne+cnt], src[:cnt])
	}
}

// quantize is vec1QuantizeVector: the bucket a vector goes in and, for a
// quantizing model, its code. tmp is tmpLen long, zero past nElem.
func (m *vec1Model) quantize(tmp, in []float32, code []byte) int {
	a := m.transform(tmp, in)
	bucket := 0
	if m.nBucket() > 0 {
		n := m.nElem()
		bucket = m.bestBucket(a[:n], false)
		if m.nCodebook() > 0 && m.residual() {
			vec1Sub(tmp[:n], a[:n], m.centroid(bucket))
			a = tmp
		}
	}
	if m.nCodebook() > 0 {
		m.encode(a, code)
	}
	return bucket
}

// quantizeN is one vector of vec1QuantizeVectorN, a rebuild's batched path.
func (m *vec1Model) quantizeN(tmp, in []float32, code []byte) int {
	n := m.nElem()
	a := m.transformN(tmp, in)
	bucket := 0
	if m.nBucket() > 0 {
		bucket = m.bestBucket(a[:n], true)
		if m.nCodebook() > 0 && m.residual() {
			vec1Sub(tmp[:n], a[:n], m.centroid(bucket))
			a = tmp
		}
	}
	if m.nCodebook() > 0 {
		if m.bitQuant() {
			m.encodeBit(a, code)
		} else {
			m.encodePQN(a, code)
		}
	}
	return bucket
}

// quantizeMany is quantizeN for a batch of vectors (a multiple of four),
// as a rebuild's jobs take them: the bucket search goes over the centroids
// once for many vectors (vec1BestMany), each codebook four vectors at a time
// (vec1Best4). tmp[i] is vector i's scratch, tmpLen long; buckets receives
// the buckets. sc is the caller's scratch.
func (m *vec1Model) quantizeMany(tmp, in [][]float32, code [][]byte, buckets []int, sc *vec1Buf) {
	n := m.nElem()
	cnt := len(in)
	a := make([][]float32, cnt)
	for i := range cnt {
		a[i] = m.transformN(tmp[i], in[i])
	}
	t8 := m.t8()
	if m.nBucket() > 0 {
		if cap(sc.pack) < cnt*n {
			sc.pack = make([]float32, cnt*n)
		}
		vecs := sc.pack[:cnt*n]
		for i := range cnt {
			copy(vecs[i*n:], a[i][:n])
		}
		if !vec1BestMany(t8.centN, m.cent, m.nBucket(), n, vecs, true, true, sc, buckets, nil) {
			for i := range cnt {
				buckets[i] = m.bestBucket(a[i][:n], true)
			}
		}
		if m.nCodebook() > 0 && m.residual() {
			for i := range cnt {
				vec1Sub(tmp[i][:n], a[i][:n], m.centroid(buckets[i]))
				a[i] = tmp[i]
			}
		}
	} else {
		clear(buckets)
	}
	if m.nCodebook() == 0 {
		return
	}
	if m.bitQuant() {
		for i := range cnt {
			m.encodeBit(a[i], code[i])
		}
		return
	}
	ne, nb := m.nCodeElem, m.nCodebook()
	w := ne * nb
	if cap(sc.pack) < 4*w {
		sc.pack = make([]float32, 4*w)
	}
	vecs := sc.pack[:4*w]
	if cap(sc.best) < 4*nb {
		sc.best = make([]int, 4*nb)
	}
	codes := sc.best[:4*nb]
	for g := 0; g < cnt; g += 4 {
		for i := range 4 {
			copy(vecs[i*w:i*w+w], a[g+i][:w])
		}
		if vec1Best4(t8.booksN, m.book, nb, vec1CodebookSize, ne, vecs, w, true, sc, codes) {
			for b := range nb {
				for i := range 4 {
					code[g+i][b] = byte(codes[4*b+i])
				}
			}
		} else {
			for i := range 4 {
				m.encodePQN(a[g+i], code[g+i])
			}
		}
	}
}

// pqLUT is vec1PQLUTBuild: for each codebook, the float32 squared distance
// from a's sub-vector to each of its 256 codes.
func (m *vec1Model) pqLUT(a []float32, lut []float32) {
	ne := m.nCodeElem
	for b, book := range m.t8().book {
		vec1T8F(jit.Vec1L2F, book, ne, a[b*ne:b*ne+ne], lut[b*vec1CodebookSize:(b+1)*vec1CodebookSize])
	}
}

// bestBucket is vec1PqBestMatch over the centroids, or with round the
// scalar vec1BestMatchN's choice (each distance rounded to float32).
func (m *vec1Model) bestBucket(a []float32, round bool) int {
	buf := vec1Bufs.Get().(*vec1Buf)
	var k int
	k, _, buf.d = vec1BestT8(m.t8().cent, m.nBucket(), m.nElem(), a, round, buf.d)
	vec1Bufs.Put(buf)
	return k
}

// rotate is vec1Rotate by the model's rotation: out[i] is row i's float32
// dot product with in.
func (m *vec1Model) rotate(in, out []float32) {
	n := m.nElem()
	rot := m.t8().rot
	buf := vec1Bufs.Get().(*vec1Buf)
	if need := 8 * vec1T8Blocks(rot, n); cap(buf.f) < need {
		buf.f = make([]float32, need)
	}
	vec1T8F(jit.Vec1DotF, rot, n, in, buf.f[:cap(buf.f)])
	copy(out[:n], buf.f[:n])
	vec1Bufs.Put(buf)
}

// bqLUT is vec1BinaryQuantLUTBuild: for each byte of a binary code, the
// value its 256 settings contribute to the dot product with a.
func (m *vec1Model) bqLUT(nElem int, a []float32, lut []float32) {
	c := float32(1.0 / float64(float32(math.Sqrt(float64(float32(nElem))))))
	ne := m.nCodeElem
	var zero [8]float32
	for b := range m.nCodebook() {
		t := lut[b*256 : b*256+256]
		sub := a[b*ne:]
		t[0] = 0
		if ne == 8 {
			for i := range 8 {
				zero[i] = float32(sub[i] * c)
				t[0] -= zero[i]
			}
		} else {
			for i := 0; i < 16; i += 2 {
				zero[i/2] = float32((sub[i] + sub[i+1]) * c)
				t[0] -= zero[i/2]
			}
		}
		next := 1
		for i := range 8 {
			add := float32(2 * zero[i])
			for j := range next {
				t[next+j] = t[j] + add
			}
			next += next
		}
	}
}

// vec1BQSize is vec1BinaryQuantSize: a binary LUT's float count.
func vec1BQSize(nElem int) int { return (nElem + 7) / 8 * 256 }

// ---- the result heap -------------------------------------------------------

// vec1Res is Vec1AnnResult.
type vec1Res struct {
	rowid int64
	dist  float64
}

// vec1Heap is Vec1AnnHeap: a max-heap of the best nMax results by distance,
// root the worst. A streaming heap keeps every candidate past nMax too,
// unordered, behind the heap.
type vec1Heap struct {
	res       []vec1Res
	nMax      int
	streaming bool
	fMin      float64
}

func newVec1Heap(nMax int, streaming bool) *vec1Heap {
	n := nMax
	if streaming {
		n = max(n, (128*1024)/16)
	}
	return &vec1Heap{res: make([]vec1Res, 0, n), nMax: nMax, streaming: streaming, fMin: math.Inf(1)}
}

func (p *vec1Heap) bubbleUp(i int) {
	for i > 0 {
		up := (i - 1) / 2
		if p.res[up].dist > p.res[i].dist {
			break
		}
		p.res[i], p.res[up] = p.res[up], p.res[i]
		i = up
	}
}

// bubbleDown repairs the heap from the root. It looks only at the first
// nMax entries, the heap proper.
func (p *vec1Heap) bubbleDown() {
	i := 0
	for {
		l, r, big := 2*i+1, 2*i+2, i
		if l < p.nMax && p.res[l].dist > p.res[big].dist {
			big = l
		}
		if r < p.nMax && p.res[r].dist > p.res[big].dist {
			big = r
		}
		if big == i {
			return
		}
		p.res[i], p.res[big] = p.res[big], p.res[i]
		i = big
	}
}

// insert is vec1HeapInsert.
func (p *vec1Heap) insert(rowid int64, dist float64) {
	switch {
	case len(p.res) < p.nMax:
		p.res = append(p.res, vec1Res{rowid, dist})
		p.bubbleUp(len(p.res) - 1)
	case p.streaming:
		p.res = append(p.res, vec1Res{rowid, dist})
		if dist < p.res[0].dist {
			last := len(p.res) - 1
			p.res[0], p.res[last] = p.res[last], p.res[0]
			p.bubbleDown()
		}
	case p.nMax == 0:
		// K=0: C compares with an entry it never allocated, and returns no
		// row whatever that holds (vec1DoKANNQuery sorts min(nRes, K)).
	case dist < p.res[0].dist:
		p.res[0] = vec1Res{rowid, dist}
		p.bubbleDown()
		p.fMin = p.res[0].dist
	}
}

// vec1SortResults is vec1AnnResultSort: vec1's top-down merge sort, stable,
// which decides the order of equal distances.
func vec1SortResults(a []vec1Res) {
	if len(a) < 2 {
		return
	}
	tmp := make([]vec1Res, len(a))
	var sort func(p, r int)
	sort = func(p, r int) {
		if r-p <= 1 {
			return
		}
		q := p + (r-p)/2
		sort(p, q)
		sort(q, r)
		i, j, k := p, q, p
		for i < q && j < r {
			if a[i].dist <= a[j].dist {
				tmp[k] = a[i]
				i++
			} else {
				tmp[k] = a[j]
				j++
			}
			k++
		}
		k += copy(tmp[k:], a[i:q])
		copy(tmp[k:], a[j:r])
		copy(a[p:r], tmp[p:r])
	}
	sort(0, len(a))
}

// vec1Bucket is Vec1BucketResult.
type vec1Bucket struct {
	bucket int
	dist   float32
}

// vec1SortBuckets is vec1BucketSort, the same merge sort.
func vec1SortBuckets(a []vec1Bucket) {
	if len(a) < 2 {
		return
	}
	tmp := make([]vec1Bucket, len(a))
	var sort func(p, r int)
	sort = func(p, r int) {
		if r-p <= 1 {
			return
		}
		q := p + (r-p)/2
		sort(p, q)
		sort(q, r)
		i, j, k := p, q, p
		for i < q && j < r {
			if a[i].dist <= a[j].dist {
				tmp[k] = a[i]
				i++
			} else {
				tmp[k] = a[j]
				j++
			}
			k++
		}
		k += copy(tmp[k:], a[i:q])
		copy(tmp[k:], a[j:r])
		copy(a[p:r], tmp[p:r])
	}
	sort(0, len(a))
}

// vec1SelectBuckets is vec1BucketSelect: quick-select by the last element,
// leaving the k nearest buckets first, in the order the partitioning leaves
// them -- which the sort that follows only partly undoes for equal distances.
func vec1SelectBuckets(a []vec1Bucket, k int) {
	left, right := 0, len(a)-1
	for {
		pivot := a[right].dist
		small := left
		for i := left; i < right; i++ {
			if a[i].dist < pivot {
				a[small], a[i] = a[i], a[small]
				small++
			}
		}
		a[small], a[right] = a[right], a[small]
		switch {
		case small == k:
			return
		case small > k:
			right = small - 1
		default:
			left = small + 1
		}
	}
}
