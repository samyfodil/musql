package engine

// vec1_train(VECTOR [, CONFIG]): the aggregate that trains a vec1 model
// (vec1TrainStep, vec1TrainFinal). Its options, checks, messages and the
// model blob are vec1's; so is each step of the training -- an OPQ rotation
// found by alternating PQ codebook training with a Procrustes step (a
// one-sided Jacobi SVD), a Walsh-Hadamard transform for the binary quantizer,
// coarse centroids by k-means seeded k-means++ style, residuals, and one PQ
// codebook of 256 codes per sub-space by k-means.
//
// Training is random in vec1 (its seeds come from sqlite3_randomness), so no
// two runs agree, in C or here: what is reproduced is the method, not a blob.
// A model trained here works in C SQLite, and the other way round, since the
// blob is the same format.

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/samyfodil/musql/internal/jit"
)

const (
	vec1QuantNone = 0
	vec1QuantPQ   = 1
	vec1QuantOPQ  = 2
	vec1QuantBQ   = 3

	vec1KMeansIter           = 30
	vec1KMeansStop           = 0.999
	vec1PQTrainMin           = vec1CodebookSize * 2
	vec1MaxCodesize          = 256
	vec1MinOPQRound          = 1
	vec1MaxOPQRound          = 32
	vec1DefaultOPQRnd        = 5
	vec1TrainMaxVecLen int64 = 64 << 30 // VEC1_TRAINING_MAXCHUNK chunks of 32MiB
)

// vec1TrainState is Vec1TrainCtx.
type vec1TrainState struct {
	init      bool
	nElem     int
	vecs      []float32
	nVec      int
	distance  int
	quant     int
	codesize  int
	nBucket   int
	opq       int
	residual  bool
	svdVerify bool
	wht       int
	nThread   int
	nOPQRound int
	progress  string
	profile   string
	nCodeElem int
	pager     *ReadOnlyPager
}

// stepVec1Train is vec1TrainStep.
func (it *aggItem) stepVec1Train(ctx *evalCtx, regs []Value) error {
	v, err := it.rowValue(aggExprArg, ctx, regs)
	if err != nil {
		return err
	}
	var cfg Value
	hasCfg := it.sepExpr != nil
	if hasCfg {
		if cfg, err = it.rowValue(aggExprSep, ctx, regs); err != nil {
			return err
		}
	}
	p := it.vec1
	if p == nil {
		p = &vec1TrainState{}
		it.vec1 = p
	}
	b := vec1BlobOf(v)
	if len(b) == 0 || len(b)%4 != 0 || (p.nElem != 0 && p.nElem != len(b)/4) {
		return fmt.Errorf("vec1_train: bad argument")
	}
	if vec1NonFinite(b) {
		return fmt.Errorf("vec1_train: non-finite value not allowed in vector")
	}
	if p.nElem == 0 {
		p.nElem = len(b) / 4
		p.distance = vec1DistL2
		p.nThread = 1
		if ctx != nil && ctx.pager != nil {
			c := ctx.pager.vec1Conn()
			p.nThread = c.nThreadValue()
			p.pager = ctx.pager
		}
		p.nOPQRound = vec1DefaultOPQRnd
		p.residual = true
		p.quant, p.codesize, p.wht, p.opq = -1, -1, -1, -1
		if hasCfg {
			if err := p.parseConfig(cfg); err != nil {
				return err
			}
		}
		p.defaults()
		p.init = true
	}
	if int64(p.nVec)*int64(p.nElem)*4 >= vec1TrainMaxVecLen {
		return fmt.Errorf("out of memory")
	}
	p.vecs = append(p.vecs, vec1Floats(b)...)
	p.nVec++
	return nil
}

// parseConfig is vec1Ann1TrainCfg over the JSON object.
func (p *vec1TrainState) parseConfig(cfg Value) error {
	opts := []string{"distance", "codesize", "nbucket", "quantizer", "nthread", "svd_verify", "opq", "progress", "residual", "wht", "nopq_round", "profile", "index"}
	cfgInt := func(o vec1JSONOpt, lo, hi int, allowZero bool) error {
		if o.typ != Int || (!(allowZero && o.i == 0) && (o.i < int64(lo) || o.i > int64(hi))) {
			zero := ""
			if allowZero {
				zero = ", or 0"
			}
			return fmt.Errorf("vec1: %s must be set to an integer value between %d and %d%s", o.key, lo, hi, zero)
		}
		return nil
	}
	cfgBool := func(o vec1JSONOpt) (bool, error) {
		if o.typ == Int && (o.i == 0 || o.i == 1) {
			return o.i != 0, nil
		}
		return false, fmt.Errorf("vec1: %s must be set to a boolean value", o.key)
	}
	var z []byte
	isNull := cfg.Typ == Null
	if !isNull {
		z = vec1BlobOf(cfg)
	}
	return vec1ParseJSONConfig(z, isNull, func(o vec1JSONOpt) error {
		i, err := vec1ConfigEnum(o.key, opts, "option")
		if err != nil {
			return err
		}
		switch i {
		case 0:
			d, err := vec1ParseDistance(o.text)
			p.distance = d
			return err
		case 1:
			err := cfgInt(o, 1, vec1MaxCodesize, true)
			p.codesize = int(int32(o.i))
			return err
		case 2:
			err := cfgInt(o, 0, vec1MaxBucket, true)
			p.nBucket = int(int32(o.i))
			return err
		case 3:
			q, err := vec1ConfigEnum(o.text, []string{"none", "pq", "opq", "bq"}, "quantizer")
			p.quant = q
			return err
		case 4:
			err := cfgInt(o, 1, vec1MaxThread, false)
			if err == nil {
				p.nThread = int(o.i)
			}
			return err
		case 5:
			b, err := cfgBool(o)
			p.svdVerify = b
			return err
		case 6:
			b, err := cfgBool(o)
			if err == nil {
				p.opq = 0
				if b {
					p.opq = 1
				}
			}
			return err
		case 7:
			p.progress = o.text
		case 8:
			b, err := cfgBool(o)
			if err == nil {
				p.residual = b
			}
			return err
		case 9:
			b, err := cfgBool(o)
			if err == nil {
				p.wht = 0
				if b {
					p.wht = 1
				}
			}
			return err
		case 10:
			err := cfgInt(o, vec1MinOPQRound, vec1MaxOPQRound, false)
			p.nOPQRound = int(int32(o.i))
			return err
		case 11:
			p.profile = o.text
		case 12:
			if !strings.EqualFold(o.text, "trained") {
				return fmt.Errorf("unrecognized index '%s', should be 'trained'", o.text)
			}
		}
		return nil
	})
}

// defaults is the rest of vec1TrainStep's first call: the quantizer, the
// transform and the code size each option leaves to default.
func (p *vec1TrainState) defaults() {
	if p.quant < 0 {
		switch {
		case p.codesize <= 0:
			p.quant = vec1QuantNone
		case p.opq > 0:
			p.quant = vec1QuantOPQ
		default:
			p.quant = vec1QuantPQ
		}
	}
	if p.wht < 0 {
		p.wht = 0
		if p.quant == vec1QuantBQ {
			p.wht = 1
		}
	}
	switch {
	case p.quant == vec1QuantNone:
		p.codesize = 0
		p.residual = false
	case p.quant == vec1QuantBQ:
		if p.codesize <= 0 || p.codesize >= (p.nElem+7)/8 {
			p.codesize = (p.nElem + 7) / 8
		} else {
			p.codesize = (p.nElem + 15) / 16
		}
	case p.codesize <= 0:
		if p.quant == vec1QuantPQ {
			p.codesize = (p.nElem + 7) / 8
		} else {
			p.codesize = 32
		}
	}
	if p.quant == vec1QuantPQ || p.quant == vec1QuantOPQ {
		p.nCodeElem = (p.nElem + p.codesize - 1) / p.codesize
		p.codesize = (p.nElem + p.nCodeElem - 1) / p.nCodeElem
	}
}

// finalizeVec1Train is vec1TrainFinal.
func (it *aggItem) finalizeVec1Train() (Value, error) {
	p := it.vec1
	if p == nil || p.nVec == 0 {
		return Value{}, fmt.Errorf("too few training vectors (have 0, require more than that)")
	}
	bPQ := p.quant == vec1QuantPQ || p.quant == vec1QuantOPQ
	if p.nBucket > 0 || bPQ {
		req := p.nBucket * 4
		if bPQ {
			req = max(vec1PQTrainMin, req)
		}
		if p.nVec < req {
			return Value{}, fmt.Errorf("too few training vectors (require %d, have %d)", req, p.nVec)
		}
	}
	tr := &vec1Trainer{p: p, workers: max(1, p.nThread)}
	if err := tr.log("vectors loaded, starting training"); err != nil {
		return Value{}, err
	}
	blob, err := tr.train()
	if err == nil {
		err = tr.log("training complete")
	}
	if err == nil {
		err = tr.logTimes()
	}
	if err != nil {
		return Value{}, err
	}
	return Value{Typ: Blob, S: blob}, nil
}

// vec1Trainer is one training run.
type vec1Trainer struct {
	p        *vec1TrainState
	workers  int
	total    int
	done     int
	times    [9]time.Duration
	started  time.Time
	logStale bool
}

// vec1Train timer slots (VEC1_TRAINING_*).
const (
	vec1TTotal = iota
	vec1TRotCodebooks
	vec1TRotCovariance
	vec1TRotJacobi
	vec1TRotVectors
	vec1TCoarseInit
	vec1TCoarseKMeans
	vec1TResiduals
	vec1TCodebooks
)

// log is vec1TrainLog: "SELECT progress(percent, msg)".
func (tr *vec1Trainer) log(msg string) error {
	p := tr.p
	if p.progress == "" {
		return nil
	}
	if p.pager == nil {
		return fmt.Errorf("%w: vec1_train's progress function with no connection", errVDBEUnsupported)
	}
	pct := 0
	if tr.total > 0 {
		pct = 100 * tr.done / tr.total
	}
	_, _, err := p.pager.QueryArgs("SELECT "+p.progress+"(?, ?)", []Value{{Typ: Int, I: int64(pct)}, {Typ: Text, S: []byte(msg)}})
	if err != nil {
		return errors.New(trimEnginePrefix(err.Error()))
	}
	return nil
}

// logTimes is vec1TrainLogTimes: the profile function called with the share
// of the time each stage took, then the times themselves.
func (tr *vec1Trainer) logTimes() error {
	p := tr.p
	if p.profile == "" {
		return nil
	}
	if p.pager == nil {
		return fmt.Errorf("%w: vec1_train's profile function with no connection", errVDBEUnsupported)
	}
	names := []string{"rot_codebooks", "rot_covariance", "rot_jacobi", "rot_vectors", "coarse_init", "coarse_lloyds", "residuals", "codebooks"}
	slots := []int{vec1TRotCodebooks, vec1TRotCovariance, vec1TRotJacobi, vec1TRotVectors, vec1TCoarseInit, vec1TCoarseKMeans, vec1TResiduals, vec1TCodebooks}
	total := float64(tr.times[vec1TTotal])
	for call := range 2 {
		var b strings.Builder
		fmt.Fprintf(&b, "SELECT \"%s\"('{", strings.ReplaceAll(p.profile, "\"", "\"\""))
		for i, n := range names {
			sep := ""
			if i > 0 {
				sep = ", "
			}
			t := float64(tr.times[slots[i]])
			if call == 0 {
				pct := 0.0
				if total > 0 {
					pct = t * 100 / total
				}
				fmt.Fprintf(&b, "%s\"%s\": %.2f", sep, n, pct)
			} else {
				fmt.Fprintf(&b, "%s\"%s\": \"%d\"", sep, n, int64(t))
			}
		}
		b.WriteString("}')")
		if _, _, err := p.pager.Query(b.String()); err != nil {
			return errors.New(trimEnginePrefix(err.Error()))
		}
	}
	return nil
}

// train is the body of vec1TrainFinal after its checks: the model blob.
func (tr *vec1Trainer) train() ([]byte, error) {
	p := tr.p
	start := time.Now()
	defer func() { tr.times[vec1TTotal] += time.Since(start) }()
	bPQ := p.quant == vec1QuantPQ || p.quant == vec1QuantOPQ
	if p.quant == vec1QuantOPQ {
		tr.total += 2 * p.nOPQRound
	}
	if p.nBucket > 0 {
		tr.total += 2
	}
	if bPQ {
		tr.total++
	}
	if tr.total == 0 {
		tr.total = 1
	}
	n := p.nElem
	if p.distance == vec1DistCos {
		for i := range p.nVec {
			vec1Normalize(p.vecs[i*n : i*n+n])
		}
	}
	nBook := 0
	if bPQ {
		nBook = p.codesize * p.nCodeElem * vec1CodebookSize
	}
	book := make([]float32, nBook)
	cent := make([]float32, p.nBucket*n)
	var rot []float32
	flags := uint32(vec1ModelIndex)
	if p.nBucket > 0 && p.residual {
		flags |= vec1ModelResidual
	}
	if p.quant == vec1QuantOPQ {
		flags |= vec1ModelRotate
		rot = make([]float32, n*n)
	}
	if p.wht != 0 {
		flags |= vec1ModelWHT
	}
	if p.quant == vec1QuantBQ {
		flags |= vec1ModelBitQuant
	}
	h := vec1Header{flags: flags, nElem: uint32(n), nCodebook: uint32(p.codesize), nBucket: uint32(p.nBucket), distance: uint32(p.distance)}

	switch {
	case p.quant == vec1QuantOPQ:
		if err := tr.trainRotation(book, rot); err != nil {
			return nil, err
		}
	case p.wht != 0:
		for i := range p.nVec {
			vec1ApplyWHT(p.vecs[i*n : i*n+n])
		}
	}
	if p.nBucket > 0 {
		if err := tr.log("k-means++ initalization for coarse quant"); err != nil {
			return nil, err
		}
		if err := tr.kmeans(p.vecs, p.nVec, n, p.nBucket, cent, true); err != nil {
			return nil, err
		}
		if bPQ && p.residual {
			if err := tr.log("calculating residuals"); err != nil {
				return nil, err
			}
			t0 := time.Now()
			tn := vec1MakeT8N(cent, 1, p.nBucket, n)
			tr.parallel(p.nVec, func(lo, hi int) {
				best := make([]int, hi-lo)
				vec1Nearest(tn, cent, p.nBucket, n, p.vecs[lo*n:hi*n], best, make([]float32, hi-lo))
				for i := lo; i < hi; i++ {
					v := p.vecs[i*n : i*n+n]
					b := best[i-lo]
					vec1Sub(v, v, cent[b*n:b*n+n])
				}
			})
			tr.times[vec1TResiduals] += time.Since(t0)
		}
	}
	if bPQ {
		if err := tr.log("PQ codebook training"); err != nil {
			return nil, err
		}
		t0 := time.Now()
		if err := tr.trainCodebooks(p.vecs, book); err != nil {
			return nil, err
		}
		tr.done++
		tr.times[vec1TCodebooks] += time.Since(t0)
	}
	out := make([]byte, vec1HeaderSize, vec1HeaderSize+4*(len(book)+len(cent)+len(rot)))
	h.write(out)
	out = append(out, vec1FloatBytes(book)...)
	out = append(out, vec1FloatBytes(cent)...)
	if rot != nil {
		out = append(out, vec1FloatBytes(rot)...)
	}
	return out, nil
}

// parallel runs fn over [0,n) split into the trainer's worker count.
func (tr *vec1Trainer) parallel(n int, fn func(lo, hi int)) {
	w := min(tr.workers, runtime.GOMAXPROCS(0), max(1, n))
	if w <= 1 {
		fn(0, n)
		return
	}
	var wg sync.WaitGroup
	lo := 0
	for j := range w {
		hi := lo + (n-lo)/(w-j)
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			fn(lo, hi)
		}(lo, hi)
		lo = hi
	}
	wg.Wait()
}

// ---- k-means ---------------------------------------------------------------------

// vec1Rand is vec1's PCG generator (vec1RandomNext): 31 random bits a call.
type vec1Rand struct{ state, inc uint64 }

func newVec1Rand(seed, id uint64) *vec1Rand {
	r := &vec1Rand{inc: id<<1 | 1}
	r.next()
	r.state += seed
	r.next()
	return r
}

func (r *vec1Rand) next() uint32 {
	old := r.state
	r.state = old*6364136223846793005 + r.inc
	x := uint32(((old >> 18) ^ old) >> 27)
	rot := uint32(old >> 59)
	return (x>>rot | x<<((-rot)&31)) & 0x7FFFFFFF
}

// kmeansInit is vec1KMeansInitOne: the first centroid at random, then by
// rounds of up to perRound at a time sampled in proportion to the squared
// distance to the nearest centroid so far (k-means||), the last tail rounds
// one at a time (k-means++).
func (tr *vec1Trainer) kmeansInit(vecs []float32, nVec, n, k, perRound, tail int, out []float32) {
	rnd := newVec1Rand(rand.Uint64()&0x7FFFFFFF, 0)
	minD := make([]float32, nVec)
	copy(out[:n], vecs[int(rnd.next())%nVec*n:][:n])
	nCent, nNew := 1, 1
	for rr := 1; rr < k; rr += nNew {
		first := nCent - nNew
		tn := vec1MakeT8N(out[first*n:nCent*n], 1, nNew, n)
		tr.parallel(nVec, func(lo, hi int) {
			best := make([]int, hi-lo)
			dist := make([]float32, hi-lo)
			vec1Nearest(tn, out[first*n:nCent*n], nNew, n, vecs[lo*n:hi*n], best, dist)
			for i := lo; i < hi; i++ {
				if d := dist[i-lo]; first == 0 || d < minD[i] {
					minD[i] = d
				}
			}
		})
		var total float64
		for _, d := range minD {
			total += float64(d)
		}
		if k-nCent <= tail {
			nNew = 1
		} else {
			nNew = min(perRound, k-tail-nCent)
		}
		if nNew == 1 {
			target := float32(total * float64(rnd.next()) / 0x7FFFFFFF)
			vv := 0
			for ; vv < nVec-1; vv++ {
				target -= minD[vv]
				if target <= 0 {
					break
				}
			}
			copy(out[nCent*n:nCent*n+n], vecs[vv*n:vv*n+n])
			nCent++
			continue
		}
		vv := int(rnd.next()) % nVec
		for range nNew {
			for {
				r := float64(rnd.next()) * total / 0x7FFFFFFF
				if r <= float64(minD[vv])*float64(nNew) {
					break
				}
				vv = (vv + 1) % nVec
			}
			copy(out[nCent*n:nCent*n+n], vecs[vv*n:vv*n+n])
			nCent++
		}
	}
}

// kmeans is vec1Ann1KMeans: the initialization, then Lloyd's iterations
// until 30 of them or a round improving the total distortion by under 0.1%.
// A centroid no vector chose keeps its place.
func (tr *vec1Trainer) kmeans(vecs []float32, nVec, n, k int, out []float32, coarse bool) error {
	t0 := time.Now()
	tr.kmeansInit(vecs, nVec, n, k, 8, 32, out)
	if coarse {
		tr.times[vec1TCoarseInit] += time.Since(t0)
		tr.done++
		if err := tr.log("k-means iterations for coarse quant"); err != nil {
			return err
		}
	}
	t0 = time.Now()
	tr.lloyd(vecs, nVec, n, k, out)
	if coarse {
		tr.times[vec1TCoarseKMeans] += time.Since(t0)
		tr.done++
	}
	return nil
}

// lloyd runs Lloyd's iterations over centroids in place.
func (tr *vec1Trainer) lloyd(vecs []float32, nVec, n, k int, cent []float32) {
	type part struct {
		sum   []float32
		count []int
		total float64
	}
	var prev float64
	for iter := 0; iter < vec1KMeansIter; iter++ {
		var mu sync.Mutex
		var parts []*part
		tn := vec1MakeT8N(cent, 1, k, n)
		tr.parallel(nVec, func(lo, hi int) {
			pt := &part{sum: make([]float32, k*n), count: make([]int, k)}
			best := make([]int, hi-lo)
			dist := make([]float32, hi-lo)
			vec1Nearest(tn, cent, k, n, vecs[lo*n:hi*n], best, dist)
			for i := lo; i < hi; i++ {
				v := vecs[i*n : i*n+n]
				b, d := best[i-lo], dist[i-lo]
				pt.total += float64(d)
				pt.count[b]++
				s := pt.sum[b*n : b*n+n]
				for j, x := range v {
					s[j] += x
				}
			}
			mu.Lock()
			parts = append(parts, pt)
			mu.Unlock()
		})
		sum := make([]float32, k*n)
		count := make([]int, k)
		var total float64
		for _, pt := range parts {
			for j := range sum {
				sum[j] += pt.sum[j]
			}
			for j := range count {
				count[j] += pt.count[j]
			}
			total += pt.total
		}
		for c := range k {
			if count[c] > 0 {
				for j := range n {
					cent[c*n+j] = sum[c*n+j] / float32(count[c])
				}
			}
		}
		if iter > 0 && total/prev >= vec1KMeansStop {
			break
		}
		prev = total
	}
}

// trainCodebooks is vec1TrainPQCodebooks: one k-means of 256 per sub-space,
// the last sub-space zero-padded where it runs past the vector.
func (tr *vec1Trainer) trainCodebooks(vecs []float32, book []float32) error {
	p := tr.p
	n, ne := p.nElem, p.nCodeElem
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, min(tr.workers, runtime.GOMAXPROCS(0))))
	sub := &vec1Trainer{p: p, workers: 1}
	for b := range p.codesize {
		wg.Add(1)
		sem <- struct{}{}
		go func(b int) {
			defer wg.Done()
			defer func() { <-sem }()
			off := b * ne
			cnt := min(ne, n-off)
			sv := make([]float32, p.nVec*ne)
			for i := range p.nVec {
				copy(sv[i*ne:i*ne+cnt], vecs[i*n+off:i*n+off+cnt])
			}
			out := book[b*ne*vec1CodebookSize : (b+1)*ne*vec1CodebookSize]
			sub.kmeansInit(sv, p.nVec, ne, vec1CodebookSize, 8, 32, out)
			sub.lloyd(sv, p.nVec, ne, vec1CodebookSize, out)
		}(b)
	}
	wg.Wait()
	return nil
}

// ---- OPQ ---------------------------------------------------------------------

// trainRotation is vec1TrainRotation: rounds of PQ codebooks trained on the
// rotated vectors, then the rotation that best maps the vectors onto their
// codes (vec1PqFindRotation), composed with the last. The training vectors
// end the rotation rotated.
func (tr *vec1Trainer) trainRotation(book, rotation []float32) error {
	p := tr.p
	n := p.nElem
	rotated := make([]float32, len(p.vecs))
	copy(rotated, p.vecs)
	r := make([]float32, n*n)
	prev := make([]float32, n*n)
	for round := range p.nOPQRound {
		if err := tr.log(fmt.Sprintf("OPQ round %d/%d", round+1, p.nOPQRound)); err != nil {
			return err
		}
		t0 := time.Now()
		if err := tr.trainCodebooks(rotated, book); err != nil {
			return err
		}
		tr.times[vec1TRotCodebooks] += time.Since(t0)
		if err := tr.findRotation(rotated, book, r); err != nil {
			return err
		}
		if round == 0 {
			copy(rotation, r)
		} else {
			copy(prev, rotation)
			vec1MatMulF(rotation, prev, r, n)
		}
		t0 = time.Now()
		t8 := vec1T8(rotation, n, n)
		tr.parallel(p.nVec, func(lo, hi int) {
			buf := make([]float32, len(t8)/n)
			for i := lo; i < hi; i++ {
				vec1T8F(jit.Vec1DotF, t8, n, p.vecs[i*n:i*n+n], buf)
				copy(rotated[i*n:i*n+n], buf)
			}
		})
		tr.times[vec1TRotVectors] += time.Since(t0)
		tr.done += 2
	}
	p.vecs = rotated
	return nil
}

// vec1CovarBatch is how many training vectors findRotation sums in float32
// before adding to the float64 covariance.
const vec1CovarBatch = 32

// findRotation is vec1PqFindRotation: M, the cross-covariance of the vectors
// with their PQ codes (column-major), then R = U V^T from M's SVD.
func (tr *vec1Trainer) findRotation(vecs, book []float32, r []float32) error {
	p := tr.p
	n, ne := p.nElem, p.nCodeElem
	t0 := time.Now()
	m := make([]float64, n*n)
	// The vectors in batches of vec1CovarBatch, each batch transposed and
	// laid out T8 (a row per component, a column per vector), so one kernel
	// call adds a batch's contribution to a row of M: the products summed in
	// float32 across the batch, then added to M in float64, as vec1's SIMD
	// builds accumulate it.
	nb := (p.nVec + vec1CovarBatch - 1) / vec1CovarBatch
	batches := make([][]float32, nb)
	tr.parallel(nb, func(lo, hi int) {
		tv := make([]float32, n*vec1CovarBatch)
		for b := lo; b < hi; b++ {
			clear(tv)
			for vv := range min(vec1CovarBatch, p.nVec-b*vec1CovarBatch) {
				v := vecs[(b*vec1CovarBatch+vv)*n:]
				for j := range n {
					tv[j*vec1CovarBatch+vv] = v[j]
				}
			}
			batches[b] = vec1T8(tv, n, vec1CovarBatch)
		}
	})
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, min(tr.workers, runtime.GOMAXPROCS(0))))
	for b := range p.codesize {
		wg.Add(1)
		sem <- struct{}{}
		go func(b int) {
			defer wg.Done()
			defer func() { <-sem }()
			off := b * ne
			kk := min(ne, n-off)
			code := book[off*vec1CodebookSize : (off+ne)*vec1CodebookSize]
			tn := vec1MakeT8N(code, 1, vec1CodebookSize, ne)
			subs := make([]float32, p.nVec*ne)
			for i := range p.nVec {
				copy(subs[i*ne:i*ne+kk], vecs[i*n+off:i*n+off+kk])
			}
			bests := make([]int, p.nVec)
			vec1Nearest(tn, code, vec1CodebookSize, ne, subs, bests, make([]float32, p.nVec))
			q := make([]float32, vec1CovarBatch)
			sums := make([]float32, len(batches[0])/vec1CovarBatch)
			for bt, tv := range batches {
				nIn := min(vec1CovarBatch, p.nVec-bt*vec1CovarBatch)
				for k := range kk {
					clear(q)
					for vv := range nIn {
						q[vv] = code[bests[bt*vec1CovarBatch+vv]*ne+k]
					}
					vec1T8F(jit.Vec1DotF, tv, vec1CovarBatch, q, sums)
					col := m[(k+off)*n : (k+off)*n+n]
					for j := range col {
						col[j] += float64(sums[j])
					}
				}
			}
		}(b)
	}
	wg.Wait()
	tr.times[vec1TRotCovariance] += time.Since(t0)

	t0 = time.Now()
	w := append([]float64(nil), m...)
	u := make([]float64, n*n)
	s := make([]float64, n)
	vt := make([]float64, n*n)
	tr.jacobiSVD(w, n, u, s, vt, 50, 1e-14)
	tr.times[vec1TRotJacobi] += time.Since(t0)
	vec1MatMul(r, u, vt, n)
	if p.svdVerify {
		return vec1SVDCheck(m, u, s, vt, n)
	}
	return nil
}

// jacobiSVD is vec1JacobiSVD: a one-sided Jacobi SVD of the column-major A
// (overwritten), its column pairs swept in vec1's parallel round-robin
// blocks.
func (tr *vec1Trainer) jacobiSVD(a []float64, d int, u, s, vt []float64, maxIter int, tol float64) {
	nJob := tr.workers
	if nJob == 1 {
		nJob = 2
	}
	nBlk := 2 * nJob
	perBlk := (d + nBlk - 1) / nBlk
	clear(vt)
	for j := range d {
		vt[j*d+j] = 1
	}
	applied := make([]int, nJob)
	for sweep := 0; sweep < maxIter; sweep++ {
		clear(applied)
		for step := range nBlk - 1 {
			var wg sync.WaitGroup
			for job := range nJob {
				wg.Add(1)
				go func(job int) {
					defer wg.Done()
					b1, b2 := vec1RoundRobinPair(nBlk, step, job)
					if b1 > b2 {
						b1, b2 = b2, b1
					}
					p0, q0 := b1*perBlk, b2*perBlk
					p1, q1 := min(p0+perBlk, d), min(q0+perBlk, d)
					for p := p0; p < p1; p++ {
						for q := q0; q < q1; q++ {
							applied[job] += vec1JacobiPair(p, q, a, d, vt, tol)
						}
					}
					if step == 0 {
						for p := p0; p < p1; p++ {
							for q := p + 1; q < p1; q++ {
								applied[job] += vec1JacobiPair(p, q, a, d, vt, tol)
							}
						}
						for p := q0; p < q1; p++ {
							for q := p + 1; q < q1; q++ {
								applied[job] += vec1JacobiPair(p, q, a, d, vt, tol)
							}
						}
					}
				}(job)
			}
			wg.Wait()
		}
		any := false
		for _, n := range applied {
			if n > 0 {
				any = true
			}
		}
		if !any {
			break
		}
	}
	// Vt was built row-major; it is returned column-major.
	for i := 0; i < d-1; i++ {
		for j := i + 1; j < d; j++ {
			vt[i*d+j], vt[j*d+i] = vt[j*d+i], vt[i*d+j]
		}
	}
	for j := range d {
		col := a[j*d : j*d+d]
		var norm float64
		for _, x := range col {
			norm += x * x
		}
		norm = math.Sqrt(norm)
		s[j] = norm
		uj := u[j*d : j*d+d]
		if norm > tol {
			for r, x := range col {
				uj[r] = x / norm
			}
		} else {
			for r := range uj {
				uj[r] = 0
			}
			uj[j] = 1
		}
	}
}

// vec1RoundRobinPair is vec1RoundRobinPair.
func vec1RoundRobinPair(n, r, k int) (int, int) {
	if k == 0 {
		return 0, n - 1 - r
	}
	a := ((n-1)+k-1-r)%(n-1) + 1
	b := ((n-2)+(n-k-1)-r)%(n-1) + 1
	return a, b
}

// vec1JacobiPair is vec1JacobiDoOnePair: columns p and q orthogonalized when
// they are not already, the rotation accumulated into Vt's rows.
func vec1JacobiPair(p, q int, a []float64, d int, vt []float64, tol float64) int {
	ap, aq := a[p*d:p*d+d], a[q*d:q*d+d]
	var alpha, beta, gamma float64
	for r := range d {
		alpha += ap[r] * ap[r]
		beta += aq[r] * aq[r]
		gamma += ap[r] * aq[r]
	}
	if gamma == 0 || math.Abs(gamma) < tol*math.Sqrt(alpha*beta) {
		return 0
	}
	zeta := (beta - alpha) / (2 * gamma)
	var t float64
	if zeta >= 0 {
		t = 1 / (zeta + math.Sqrt(1+zeta*zeta))
	} else {
		t = -1 / (-zeta + math.Sqrt(1+zeta*zeta))
	}
	c := 1 / math.Sqrt(1+t*t)
	s := -c * t
	for r := range d {
		wp, wq := ap[r], aq[r]
		ap[r] = c*wp + s*wq
		aq[r] = -s*wp + c*wq
	}
	vp, vq := vt[p*d:p*d+d], vt[q*d:q*d+d]
	for r := range d {
		x, y := vp[r], vq[r]
		vp[r] = c*x + s*y
		vq[r] = -s*x + c*y
	}
	return 1
}

// vec1MatMul is vec1MatrixMultiply: out = a1 a2, column-major, each product
// rounded to float32 as it is added.
func vec1MatMul(out []float32, a1, a2 []float64, n int) {
	clear(out)
	for j := range n {
		for k := range n {
			b := a2[k+j*n]
			for i := range n {
				out[i+j*n] += float32(a1[i+k*n] * b)
			}
		}
	}
}

// vec1MatMulF is vec1MatrixMultiplyF, over float32 matrices.
func vec1MatMulF(out, a1, a2 []float32, n int) {
	clear(out)
	for j := range n {
		for k := range n {
			b := a2[k+j*n]
			for i := range n {
				out[i+j*n] += float32(a1[i+k*n] * b)
			}
		}
	}
}

// vec1SVDCheck is vec1SVDSelfTest: U and V^T orthogonal and U S V^T = A,
// each to 1e-5.
func vec1SVDCheck(a, u, s, vt []float64, n int) error {
	orth := func(m []float64) float64 {
		var e float64
		for i := range n {
			for j := range n {
				var dot float64
				for r := range n {
					dot += m[i*n+r] * m[j*n+r]
				}
				want := 0.0
				if i == j {
					want = 1
				}
				e += (dot - want) * (dot - want)
			}
		}
		return math.Sqrt(e)
	}
	var e, nrm float64
	for row := range n {
		for col := range n {
			var v float64
			for k := range n {
				v += u[k*n+row] * s[k] * vt[col*n+k]
			}
			x := a[col*n+row]
			e += (x - v) * (x - v)
			nrm += x * x
		}
	}
	ou, ov, rec := orth(u), orth(vt), math.Sqrt(e/nrm)
	if ou > 0.00001 || ov > 0.00001 || rec > 0.00001 {
		s, err := fnPrintf("vec1: SVD error - fOrthU=%f, fOrthVT=%f, fReconstruction=%f",
			[]Value{{Typ: Float, F: ou}, {Typ: Float, F: ov}, {Typ: Float, F: rec}})
		if err != nil {
			return err
		}
		return errors.New(string(s.S))
	}
	return nil
}
