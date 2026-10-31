package replication

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samyfodil/musql/engine"
)

// A node that holds nothing yet takes a peer's whole database as one file, an
// image, instead of replaying the peer's history op by op: the donor writes a
// consistent copy (VACUUM INTO), streams it, and the receiver installs it under
// its own identity, then pulls the ops after the copy's cut as any node does.
// The image carries the donor's bookkeeping as it is -- op log, clock, floors,
// ranges, withheld rows, catalog -- so the receiver holds what the donor held.

// metaLayout is the _repl_meta key naming the layout of the bookkeeping tables,
// absent while it is bookkeepingLayout.
const metaLayout = "layout"

// bookkeepingLayout is the layout of the bookkeeping tables this build writes.
const bookkeepingLayout = "1"

// imageMinOps is how far a donor must be ahead for an image: less is pulled op
// by op, in about as many round trips as an image takes.
var imageMinOps uint64 = opsPerFetch

// imageCompactRatio: an image whose delta is more than 1/imageCompactRatio of
// it is compacted before it is adopted. A deliberate simplification between two
// measured points: at 100k rows the delta was 1% of the image and compacting
// it first cost 0.1-0.3 s of 2.2; at 1M it was 13% and saved 6 s of 25.
var imageCompactRatio int64 = 16

// imagePinGrace is how long a donor keeps the ops after an image's cut once the
// image is sent: the receiver pulls them after installing it.
const imagePinGrace = 30 * time.Second

// ErrImageRefused is a peer that will not send an image this node can install:
// another layout or file format, or no image to give.
var ErrImageRefused = errors.New("replication: image refused")

// errNotEmpty is an image refused because this node holds something of its own.
var errNotEmpty = errors.New("replication: this node is not empty")

// ImageRequest is what a node asks a peer for an image with: the formats it
// reads.
type ImageRequest struct {
	Layout                                    string
	FileVersion, MinFileVersion, DeltaVersion uint32
}

// ImageManifest describes an image: its formats, its Size bytes -- a segment
// file, then the first DeltaSize bytes of its delta -- whose CRC-32C is
// Checksum, and the state at its cut. VV is a lower bound of the image's
// version vector, and MaxHLC an upper bound of every HLC in it.
type ImageManifest struct {
	Site                      string
	FileVersion, DeltaVersion uint32
	Layout                    string
	Catalog                   string
	Size, DeltaSize           int64
	Checksum                  uint32
	VV, Floors                VersionVector
	Ranges                    map[string]uint64
	MaxHLC                    HLC
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func imageRequest(layout string) ImageRequest {
	file, minFile, delta := engine.FormatVersions()
	return ImageRequest{Layout: layout, FileVersion: file, MinFileVersion: minFile, DeltaVersion: delta}
}

// accepts reports why an image of m cannot be installed by a node asking with
// r, or nil.
func (r ImageRequest) accepts(m ImageManifest) error {
	if m.Layout != r.Layout || m.FileVersion < r.MinFileVersion || m.FileVersion > r.FileVersion || m.DeltaVersion > r.DeltaVersion {
		return fmt.Errorf("%w: layout %s, file format %d, delta format %d; this node reads layout %s, file formats %d to %d, delta formats to %d",
			ErrImageRefused, m.Layout, m.FileVersion, m.DeltaVersion, r.Layout, r.MinFileVersion, r.FileVersion, r.DeltaVersion)
	}
	return nil
}

// layoutOf is the bookkeeping layout of q's database.
func layoutOf(ctx context.Context, q dbtx) (string, error) {
	var v []byte
	err := q.QueryRowContext(ctx, `SELECT v FROM main._repl_meta WHERE k=?`, metaLayout).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return bookkeepingLayout, nil
	}
	return string(v), err
}

// mainFile is the path of db's main database file.
func mainFile(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA database_list`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", err
		}
		if name == "main" && file != "" {
			return file, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return "", errors.New("replication: the database has no file")
}

func commitCounter(path string) (uint32, error) {
	var ctr uint32
	err := engine.CommitState(path, func(c engine.CommitInfo) error { ctr = c.Counter; return nil })
	return ctr, err
}

// --- donor ---

// imagePin holds a donor's prune at an image's cut: the receiver pulls the ops
// after it once the image is installed.
type imagePin struct {
	vv    VersionVector
	until time.Time // zero while the image is being sent
}

// livePins forgets the expired pins and returns the others. s.mu is held.
func (s *Store) livePins() []*imagePin {
	now := time.Now()
	live := s.pins[:0]
	for _, p := range s.pins {
		if p.until.IsZero() || now.Before(p.until) {
			live = append(live, p)
		}
	}
	clear(s.pins[len(live):])
	s.pins = live
	return live
}

// pinnedFrontier lowers frontier to every live pin's cut. s.mu is held.
func (s *Store) pinnedFrontier(frontier VersionVector) {
	for _, p := range s.livePins() {
		for site, f := range frontier {
			frontier[site] = min(f, p.vv[site])
		}
	}
}

// imageBody is an image being read: a segment file, then the first bytes of
// its delta. Close removes the image and releases its pin.
type imageBody struct {
	io.Reader
	files   []*os.File
	release func()
}

func (b imageBody) Close() error {
	var err error
	for _, f := range b.files {
		err = errors.Join(err, f.Close())
	}
	b.release()
	return err
}

// imageCutForTest runs once an image is cut, before it is sent.
var imageCutForTest func()

// deltaSuffix names a segment file's delta (engine.CommitInfo).
const deltaSuffix = ".delta"

// Image returns a consistent copy of the database, for a node that holds
// nothing yet: its manifest and its bytes, which closing removes. Until then,
// and imagePinGrace after, prune keeps every op after its cut. The copy is the
// committed files themselves, linked at the cut, or, where they cannot be or
// the node asking cannot read their formats, a copy the database writes. A
// request for another layout or file format is ErrImageRefused.
func (s *Store) Image(ctx context.Context, req ImageRequest) (ImageManifest, io.ReadCloser, error) {
	layout, err := layoutOf(ctx, s.db)
	if err != nil {
		return ImageManifest{}, nil, err
	}
	m := ImageManifest{Site: s.site, Layout: layout}
	m.FileVersion, _, _ = engine.FormatVersions()
	if layout != req.Layout {
		return ImageManifest{}, nil, req.accepts(m)
	}
	path, err := mainFile(ctx, s.db)
	if err != nil {
		return ImageManifest{}, nil, err
	}
	dir, err := os.MkdirTemp(filepath.Dir(path), filepath.Base(path)+imageDirInfix)
	if err != nil {
		return ImageManifest{}, nil, err
	}
	s.mu.Lock()
	pin := &imagePin{vv: s.vv.Clone()}
	s.pins = append(s.livePins(), pin)
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		pin.until = time.Now().Add(imagePinGrace)
		s.mu.Unlock()
		os.RemoveAll(dir)
	}
	m.VV = pin.vv
	body, err := s.cutImage(ctx, path, dir, req, &m)
	if err != nil {
		release()
		return ImageManifest{}, nil, err
	}
	body.release = release
	return m, body, nil
}

// cutImage pins path's committed files, or failing that writes a copy of the
// database into dir, fills in what m says of the image, and opens it for
// reading.
//
// What m says of pinned files is read from the database beforehand: the
// catalog the image's must equal, and floors and ranges that may be lower than
// its own.
func (s *Store) cutImage(ctx context.Context, path, dir string, req ImageRequest, m *ImageManifest) (imageBody, error) {
	if err := imageState(ctx, s.db, m); err != nil {
		return imageBody{}, err
	}
	p, err := engine.PinCommittedFiles(path, dir)
	if err == nil {
		m.FileVersion, m.DeltaVersion = p.FileVersion, p.DeltaVersion
		if req.accepts(*m) == nil {
			return s.openImage(p.Seg, p.SegSize, p.Delta, p.DeltaLen, m)
		}
		p.Close()
	} else {
		log.Printf("replication: pinning an image's files: %v; writing a copy instead", err)
	}
	m.FileVersion, _, _ = engine.FormatVersions()
	m.DeltaVersion = 0
	if err := req.accepts(*m); err != nil {
		return imageBody{}, err
	}
	img := filepath.Join(dir, "image.musq")
	src, err := openApplyDB(path)
	if err != nil {
		return imageBody{}, err
	}
	_, err = src.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(img, "'", "''")+`'`)
	src.Close()
	if err != nil {
		return imageBody{}, err
	}
	db, err := openApplyDB(img)
	if err != nil {
		return imageBody{}, err
	}
	err = imageState(ctx, db, m)
	db.Close()
	if err != nil {
		return imageBody{}, err
	}
	f, err := os.Open(img)
	if err != nil {
		return imageBody{}, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return imageBody{}, err
	}
	return s.openImage(f, fi.Size(), nil, 0, m)
}

// openImage reads the image of segment file seg, segSize bytes, and the first
// n bytes of delta, if any, and fills in m's cut, size and checksum.
func (s *Store) openImage(seg *os.File, segSize int64, delta *os.File, n int64, m *ImageManifest) (imageBody, error) {
	b := imageBody{files: []*os.File{seg}, release: func() {}}
	parts := func() []io.Reader {
		r := []io.Reader{io.NewSectionReader(seg, 0, segSize)}
		if delta != nil {
			r = append(r, io.NewSectionReader(delta, 0, n))
		}
		return r
	}
	if delta != nil {
		b.files = append(b.files, delta)
	}
	m.MaxHLC = s.clock.Now()
	if imageCutForTest != nil {
		imageCutForTest()
	}
	h := crc32.New(castagnoli)
	size, err := io.CopyBuffer(h, io.MultiReader(parts()...), make([]byte, 1<<20))
	if err != nil {
		b.Close()
		return imageBody{}, err
	}
	m.Size, m.Checksum = size, h.Sum32()
	if delta != nil {
		m.DeltaSize = n
	}
	b.Reader = io.MultiReader(parts()...)
	return b, nil
}

// imageState reads into m the catalog, floors and ranges of the image db.
func imageState(ctx context.Context, db *sql.DB, m *ImageManifest) error {
	var ver []byte
	if err := db.QueryRowContext(ctx, `SELECT v FROM main._repl_meta WHERE k=?`, metaSchemaVer).Scan(&ver); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	m.Catalog = string(ver)
	m.Floors, m.Ranges = VersionVector{}, map[string]uint64{}
	rows, err := db.QueryContext(ctx, `SELECT site, seq FROM main._repl_floors`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var site string
		var seq uint64
		if err := rows.Scan(&site, &seq); err != nil {
			rows.Close()
			return err
		}
		m.Floors[site] = seq
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if rows, err = db.QueryContext(ctx, `SELECT lo, site FROM main._repl_ranges`); err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var lo uint64
		var site string
		if err := rows.Scan(&lo, &site); err != nil {
			return err
		}
		m.Ranges[site] = lo
	}
	return rows.Err()
}

// --- receiver ---

// The files an image leaves beside the database while it is made or installed:
// a donor's directory of the copy it sends, a receiver's staged copy, and the
// marker that says an install is under way.
const (
	imageDirInfix = ".image-"
	stagedInfix   = ".boot-"
	markerSuffix  = ".boot"
)

// imageStagedForTest runs once an install's image is staged and verified; an
// error ends the install there.
var imageStagedForTest func() error

// imageTries is how many rounds in a row an empty node tries an image that
// fails before it pulls op by op instead. A refusal is not retried.
const imageTries = 3

// bootImage installs a peer's image when this node holds nothing yet. It
// reports whether one failed in a way the next round may not, so the round
// should wait for it rather than pull op by op.
func (n *Node) bootImage(ctx context.Context, peers []PeerID) (retry bool) {
	im, ok := n.tr.(Imager)
	if n.syncer == nil || !ok || !n.store.mayBeEmpty() {
		return false
	}
	failed := false
	for _, peer := range peers {
		mine := n.store.LocalVV()
		vv, err := n.tr.ExchangeVV(ctx, peer, mine)
		if err != nil || aheadOf(vv, mine, n.store.Site()) < imageMinOps {
			continue
		}
		err = n.syncer.installImage(ctx, im, peer)
		if err == nil {
			if own := n.store.LocalVV()[n.store.Site()]; own > n.seq.Load() {
				n.seq.Store(own)
			}
			n.imageFails = 0
			return false
		}
		log.Printf("replication: image from %s: %v", peer, err)
		switch {
		case errors.Is(err, errNotEmpty):
			return false
		case !errors.Is(err, ErrImageRefused):
			failed = true
		}
	}
	if !failed {
		return false
	}
	n.imageFails++
	return n.imageFails < imageTries
}

// aheadOf is how many ops of other sites than self vv holds past mine.
func aheadOf(vv, mine VersionVector, self string) uint64 {
	var n uint64
	for site, seq := range vv {
		if site != self && seq > mine[site] {
			n += seq - mine[site]
		}
	}
	return n
}

// mayBeEmpty is the in-memory half of emptyOwnOps: no user object, and no op of
// another site in the version vector or below a floor. Its own floor is no
// matter: a lone node prunes its own ops, and the mode ops it may hold are
// never pruned.
func (s *Store) mayBeEmpty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emptyInMemory()
}

func (s *Store) emptyInMemory() bool {
	if len(s.cat.Objs) > 0 {
		return false
	}
	for _, vv := range []VersionVector{s.vv, s.floors} {
		for site, seq := range vv {
			if site != s.site && seq > 0 {
				return false
			}
		}
	}
	return true
}

// emptyOwnOps returns this site's ops when the database holds nothing an image
// would lose: no user object, no other site's floor, of its own ops only mode ops, and of
// other sites' only ops the version vector does not count yet -- pushed ahead of
// the history they follow, they are still at their origin, which keeps them
// until this node acks them. Otherwise it is errNotEmpty. s.mu is held.
func (s *Store) emptyOwnOps(ctx context.Context) ([]Op, error) {
	if !s.emptyInMemory() {
		return nil, errNotEmpty
	}
	var objs int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM main.sqlite_master WHERE tbl_name NOT IN `+
		`('_repl_meta', '_repl_oplog', '_repl_clock', '_repl_ranges', '_repl_withheld', '_repl_floors') AND tbl_name NOT LIKE 'sqlite_%'`).Scan(&objs); err != nil {
		return nil, err
	}
	if objs > 0 {
		return nil, errNotEmpty
	}
	rows, err := s.db.QueryContext(ctx, `SELECT site, seq, hlc, tbl, pk, op, cells FROM main._repl_oplog WHERE site=? ORDER BY seq`, s.site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var own []Op
	for rows.Next() {
		op, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		if op.Kind != OpSchema {
			return nil, errNotEmpty
		}
		if so, err := decodeSchemaOp(op); err != nil || so.Act != actMode {
			return nil, errNotEmpty
		}
		own = append(own, op)
	}
	return own, rows.Err()
}

// installImage takes peer's image and installs it in place of this node's
// database, which holds nothing yet.
//
// The image is staged beside the database and verified, then made this node's:
// its own site id, its own rowid range, and its own ops so far (mode ops). Then,
// with the store's lock held and nothing committed since its emptiness was
// checked, it is renamed in as a commit after the database's last, so every
// connection reloads. A marker names the staged file meanwhile, and Open
// finishes an install a crash interrupted.
func (sy *Syncer) installImage(ctx context.Context, im Imager, peer PeerID) error {
	s := sy.store
	path, err := mainFile(ctx, s.db)
	if err != nil {
		return err
	}
	layout, err := layoutOf(ctx, s.db)
	if err != nil {
		return err
	}
	req := imageRequest(layout)
	m, body, err := im.GetImage(ctx, peer, req)
	if err != nil {
		return err
	}
	if err := req.accepts(m); err != nil {
		body.Close()
		return err
	}
	staged, err := stageImage(path, m, body)
	if err != nil {
		return err
	}
	pending := false
	defer func() {
		if !pending {
			engine.RemoveDatabaseFiles(staged)
		}
	}()
	if imageStagedForTest != nil {
		if err := imageStagedForTest(); err != nil {
			return err
		}
	}
	if m.DeltaSize > 0 && m.DeltaSize*imageCompactRatio > m.Size {
		// Every open of the staged file replays its delta, and the adopt below
		// opens it: a big delta is cheaper rewritten once, outside the lock.
		if _, err := engine.CompactSegmentFile(staged); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctr, err := commitCounter(path)
	if err != nil {
		return err
	}
	own, err := s.emptyOwnOps(ctx)
	if err != nil {
		return err
	}
	if err := adoptImage(ctx, staged, m, sy.site, sy.mode, own); err != nil {
		return err
	}
	ownSeq := uint64(0)
	if len(own) > 0 {
		ownSeq = own[len(own)-1].Seq
	}
	// The compaction makes the staged image durable: a new file, synced before
	// it is renamed in.
	if done, err := engine.CompactSegmentFile(staged); err != nil {
		return err
	} else if !done {
		return fmt.Errorf("replication: the staged image %s was not compacted", staged)
	}
	marker := path + markerSuffix
	if err := writeDurable(marker, []byte(filepath.Base(staged))); err != nil {
		return err
	}
	if err := engine.InstallSegmentFile(path, staged, ctr); err != nil {
		if pending, err = keepMarker(path, staged, ctr, err); !pending {
			return err
		}
		// Part way: finish it as Open would, or leave it to Open.
		if ferr := finishInstall(path); ferr != nil {
			return errors.Join(err, ferr)
		}
		pending = false
	}
	// The database is the image now, and the store must follow it even if ctx
	// ends: a store left as it was would go on as if the database were empty.
	ctx = context.WithoutCancel(ctx)
	s.clock.Merge(m.MaxHLC)
	if err := s.reloadLocked(ctx); err != nil {
		return err
	}
	if err := removeDurable(marker); err != nil {
		log.Printf("replication: %v (Open removes it)", err)
	}
	// A site the image knows from more than the mode ops carried in may have
	// rows in its range, deleted ones too, whose rowids it must not hand out
	// again. A new site has none, and its scan of the clock is skipped; a
	// donor that pruned to its ack has a floor for it at those mode ops.
	if s.floors[s.site] > ownSeq || s.vv[s.site] > ownSeq {
		high, err := s.rowidHighs(ctx, sy.rowidLo, sy.rowidHi)
		if err != nil {
			return err
		}
		sy.highMu.Lock()
		sy.high = high
		sy.highMu.Unlock()
	}
	sy.setCatalog(s.cat, s.ver)
	sy.images.Add(1)
	log.Printf("replication: installed an image of %d bytes (%d of delta) from %s", m.Size, m.DeltaSize, peer)
	return nil
}

// keepMarker ends an install that failed once its marker was written: when the
// database is as it was, the marker goes and err is all; otherwise the marker
// and the staged image stay, for Open to finish the install, and pending is set.
func keepMarker(path, staged string, ctr uint32, err error) (pending bool, _ error) {
	if _, serr := os.Stat(staged); serr == nil {
		// A commit since the counter was read is the database's own: the
		// install did nothing, and finishing it would lose that commit.
		if now, cerr := commitCounter(path); errors.Is(err, engine.ErrStateMoved) || (cerr == nil && now == ctr) {
			if rerr := removeDurable(path + markerSuffix); rerr != nil {
				return true, errors.Join(err, rerr)
			}
			return false, err
		}
	}
	return true, fmt.Errorf("%w (the install finishes when the database is next opened)", err)
}

// stageImage copies body, which m describes, to a new segment file and its
// delta beside path, and checks their size and checksum.
func stageImage(path string, m ImageManifest, body io.ReadCloser) (string, error) {
	defer body.Close()
	if m.DeltaSize < 0 || m.DeltaSize > m.Size {
		return "", fmt.Errorf("replication: image of %d bytes with a delta of %d", m.Size, m.DeltaSize)
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+stagedInfix+"*")
	if err != nil {
		return "", err
	}
	staged := f.Name()
	h := crc32.New(castagnoli)
	buf := make([]byte, 1<<20)
	part := func(w io.Writer, n int64) (int64, error) {
		return io.CopyBuffer(io.MultiWriter(w, h), io.LimitReader(body, n), buf)
	}
	n, err := part(f, m.Size-m.DeltaSize)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && m.DeltaSize > 0 {
		var d *os.File
		if d, err = os.OpenFile(staged+deltaSuffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			var k int64
			k, err = part(d, m.DeltaSize)
			n += k
			if cerr := d.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err == nil {
		var extra int64
		extra, err = io.CopyN(io.Discard, body, 1)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		n += extra
	}
	if err == nil && (n != m.Size || h.Sum32() != m.Checksum) {
		err = fmt.Errorf("replication: image of %d bytes does not match its manifest (%d bytes)", n, m.Size)
	}
	if err != nil {
		engine.RemoveDatabaseFiles(staged)
		return "", err
	}
	return staged, nil
}

// adoptImage makes the staged image this node's: it checks that the image is
// the one m describes, in this node's mode, then gives it this node's site id,
// claims its rowid range, and adds its own ops.
func adoptImage(ctx context.Context, staged string, m ImageManifest, site, mode string, own []Op) error {
	db, err := openApplyDB(staged)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var kept, ver []byte
	if err := db.QueryRowContext(ctx, `SELECT v FROM main._repl_meta WHERE k='site'`).Scan(&kept); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT v FROM main._repl_meta WHERE k=?`, metaSchemaVer).Scan(&ver); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	layout, err := layoutOf(ctx, db)
	if err != nil {
		return err
	}
	cat, _, _, err := loadCatalog(ctx, db)
	if err != nil {
		return err
	}
	switch {
	case string(kept) != m.Site || string(ver) != m.Catalog || layout != m.Layout:
		return fmt.Errorf("replication: the image is not what its manifest says (site %q, catalog %q, layout %s)", kept, ver, layout)
	case string(kept) == site:
		return fmt.Errorf("replication: the image is this node's own site %q", site)
	case cat == nil || cat.Mode != mode:
		return fmt.Errorf("%w: the image's database does not replicate in %s mode", ErrImageRefused, mode)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := adoptTx(ctx, tx, site, own); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return setGuard(ctx, db)
}

func adoptTx(ctx context.Context, tx *sql.Tx, site string, own []Op) error {
	lo, _ := RowidRange(site)
	var owner string
	switch err := tx.QueryRowContext(ctx, `SELECT site FROM main._repl_ranges WHERE lo=?`, int64(lo)).Scan(&owner); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO main._repl_ranges(lo, site) VALUES(?, ?)`, int64(lo), site); err != nil {
			return err
		}
	case err != nil:
		return err
	case owner != site:
		return fmt.Errorf("%w: site %q holds this node's rowid range there", ErrImageRefused, owner)
	}
	for _, op := range own {
		var hlc, kind int64
		var tbl string
		var pk, cells []byte
		err := tx.QueryRowContext(ctx, `SELECT hlc, tbl, pk, op, cells FROM main._repl_oplog WHERE site=? AND seq=?`, op.Site, op.Seq).Scan(&hlc, &tbl, &pk, &kind, &cells)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `INSERT INTO main._repl_oplog(site,seq,hlc,tbl,pk,op,cells) VALUES(?,?,?,?,?,?,?)`,
				op.Site, op.Seq, int64(op.HLC), op.Tbl, op.PK, int64(op.Kind), encodeCells(op.Cells)); err != nil {
				return err
			}
		case err != nil:
			return err
		case HLC(hlc) != op.HLC || tbl != op.Tbl || !bytes.Equal(pk, op.PK) || OpKind(kind) != op.Kind || !bytes.Equal(cells, encodeCells(op.Cells)):
			return fmt.Errorf("%w: it holds another op %s/%d", ErrImageRefused, op.Site, op.Seq)
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE main._repl_meta SET v=? WHERE k='site'`, []byte(site))
	return err
}

// reloadLocked reads the store's state again from its database, which an image
// has replaced. s.mu is held.
func (s *Store) reloadLocked(ctx context.Context) error {
	fresh, err := OpenStore(ctx, s.db, s.site)
	if err != nil {
		return err
	}
	s.vv, s.floors, s.cat, s.ver, s.mayHold = fresh.vv, fresh.floors, fresh.cat, fresh.ver, fresh.mayHold
	s.selfAck, s.txHeld = Ack{}, nil
	s.ownSeq.Store(fresh.ownSeq.Load())
	s.clock.Merge(fresh.clock.Now())
	return nil
}

// recoverImage finishes an install a crash interrupted, from its marker, and
// removes what interrupted images left beside the database at path. Open runs
// it, before anything else has the database.
func recoverImage(path string) error {
	if err := finishInstall(path); err != nil {
		return err
	}
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	base := filepath.Base(path)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), base+stagedInfix) || strings.HasPrefix(e.Name(), base+imageDirInfix) {
			os.RemoveAll(filepath.Join(filepath.Dir(path), e.Name()))
		}
	}
	return nil
}

// finishInstall finishes the install path's marker names, if there is one: the
// staged image still there is installed, and the marker goes.
func finishInstall(path string) error {
	marker := path + markerSuffix
	b, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	staged := filepath.Join(filepath.Dir(path), string(b))
	if _, err := os.Stat(staged); err == nil {
		ctr, err := commitCounter(path)
		if err != nil {
			return err
		}
		if err := engine.InstallSegmentFile(path, staged, ctr); err != nil {
			return fmt.Errorf("replication: finishing an image install: %w", err)
		}
		engine.RemoveDatabaseFiles(staged)
	}
	return removeDurable(marker)
}

// writeDurable writes b to path and makes the file and its name durable.
func writeDurable(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDirOf(path)
}

func removeDurable(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirOf(path)
}

func syncDirOf(path string) error {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	d.Sync() // some filesystems refuse a directory fsync; the rename is still ordered
	return nil
}

// ImageInstalled reports whether db's node took its database from a peer's
// image rather than op by op. db is the *sql.DB Open returned.
func ImageInstalled(db *sql.DB) bool {
	v, ok := openDBs.Load(db)
	return ok && v.(*Syncer).images.Load() > 0
}
