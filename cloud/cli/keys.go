package cli

import (
	"bytes"
	"io"
)

// keyAliases are the other spellings terminals send for keys the line editor
// knows only as ESC [ x: application cursor mode's ESC O x, and the VT220,
// rxvt and Linux console Home and End. Unknown, ESC O H was read as an unknown
// "ESC O" and a typed "H" (#132).
var keyAliases = []struct{ from, to []byte }{
	{[]byte("\x1bOH"), []byte("\x1b[H")},
	{[]byte("\x1bOF"), []byte("\x1b[F")},
	{[]byte("\x1bOA"), []byte("\x1b[A")},
	{[]byte("\x1bOB"), []byte("\x1b[B")},
	{[]byte("\x1bOC"), []byte("\x1b[C")},
	{[]byte("\x1bOD"), []byte("\x1b[D")},
	{[]byte("\x1b[1~"), []byte("\x1b[H")},
	{[]byte("\x1b[7~"), []byte("\x1b[H")},
	{[]byte("\x1b[4~"), []byte("\x1b[F")},
	{[]byte("\x1b[8~"), []byte("\x1b[F")},
}

// keyReader rewrites keyAliases in what the terminal sends, holding back a
// read's unfinished escape sequence until the next read completes it.
type keyReader struct {
	r       io.Reader
	pending []byte
	out     []byte
}

func (k *keyReader) Read(p []byte) (int, error) {
	for len(k.out) == 0 {
		buf := make([]byte, 256)
		n, err := k.r.Read(buf)
		data := append(k.pending, buf[:n]...)
		k.pending = nil
		if err == nil {
			if i := partialAlias(data); i >= 0 {
				k.pending, data = append([]byte(nil), data[i:]...), data[:i]
			}
		}
		k.out = rewriteKeys(data)
		if err != nil {
			if len(k.out) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, k.out)
	k.out = k.out[n:]
	return n, nil
}

func rewriteKeys(b []byte) []byte {
	for _, a := range keyAliases {
		b = bytes.ReplaceAll(b, a.from, a.to)
	}
	return b
}

// partialAlias is where a key alias begins at b's end, cut short, or -1.
func partialAlias(b []byte) int {
	i := bytes.LastIndexByte(b, 0x1b)
	if i < 0 {
		return -1
	}
	tail := b[i:]
	for _, a := range keyAliases {
		if len(tail) < len(a.from) && bytes.HasPrefix(a.from, tail) {
			return i
		}
	}
	return -1
}
