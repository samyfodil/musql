//go:build darwin

package mmapfile

// readNotFault: on macOS the first touch of a mapped page is a page-in of its
// own even when the file is in the page cache. Traced on an M4 with the vec1
// benchmark's fault counts: a 1M-row search re-ranking 100 vectors scattered
// over a 1.5 GB segment took about 110 major faults a query (22,410 over 200
// queries; C SQLite + vec1, which reads with pread, 3) and 15 ms a query.
const readNotFault = true
