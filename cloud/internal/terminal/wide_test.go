package terminal

import (
	"strconv"
	"strings"
	"testing"
)

// screen is a terminal the editor's output is played on: a cell per column,
// a wide character taking two, the cursor moved by CR, LF and CSI A-D, and a
// write past the last column wrapping first.
type screen struct {
	w, x, y int
	rows    map[int][]rune
}

func (s *screen) put(r rune) {
	cols := runeWidth(r)
	if s.x+cols > s.w {
		s.x, s.y = 0, s.y+1
	}
	row := s.rows[s.y]
	for len(row) < s.x+cols {
		row = append(row, ' ')
	}
	row[s.x] = r
	if cols == 2 {
		row[s.x+1] = 0
	}
	s.rows[s.y] = row
	s.x += cols
}

func (s *screen) play(out string) {
	rs := []rune(out)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; r {
		case '\r':
			s.x = 0
		case '\n':
			s.y++
		case keyEscape:
			j := i + 2 // past ESC [
			for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
				j++
			}
			n := 1
			if j > i+2 {
				n, _ = strconv.Atoi(string(rs[i+2 : j]))
			}
			switch rs[j] {
			case 'A':
				s.y -= n
			case 'B':
				s.y += n
			case 'C':
				s.x += n
			case 'D':
				s.x -= n
			}
			i = j
		default:
			s.put(r)
		}
	}
}

func (s *screen) row(y int) string {
	return strings.TrimRight(strings.ReplaceAll(string(s.rows[y]), "\x00", ""), " ")
}

// TestWideCharacters: with wide characters on the line, the cursor lands
// where the editor means it to and the screen shows the line (#218).
func TestWideCharacters(t *testing.T) {
	const (
		left, right, home, end = "\x1b[D", "\x1b[C", "\x1b[H", "\x1b[F"
		backspace, up          = "\x7f", "\x1b[A"
	)
	for _, tc := range []struct {
		name, keys string
		width      int
		x, y       int
		rows       []string
	}{
		{"typed", "中文。", 80, 8, 0, []string{"> 中文。"}},
		{"left", "中文。" + left + left + left, 80, 2, 0, []string{"> 中文。"}},
		{"left right", "中文。" + left + left + left + right, 80, 4, 0, []string{"> 中文。"}},
		{"backspace all", "中文。" + backspace + backspace + backspace, 80, 2, 0, []string{">"}},
		{"backspace then type", "中文。" + backspace + backspace + backspace + "ab", 80, 4, 0, []string{"> ab"}},
		{"home end", "ab中" + home + end, 80, 6, 0, []string{"> ab中"}},
		{"home then type", "中文" + home + "x", 80, 3, 0, []string{"> x中文"}},
		{"wrap", "中文中文中", 10, 2, 1, []string{"> 中文中文", "中"}},
		{"wrap odd column", "a中文中文", 10, 2, 1, []string{"> a中文中", "文"}},
		{"wrap odd column home end", "a中文中文" + home + end, 10, 2, 1, []string{"> a中文中", "文"}},
		{"wrap odd column backspace", "a中文中文" + backspace + backspace, 10, 7, 0, []string{"> a中文", ""}},
		{"wrap home", "中文中文中" + home, 10, 2, 0, []string{"> 中文中文", "中"}},
		{"history", "中文中\r" + "a" + up, 80, 8, 1, []string{"> 中文中", "> 中文中"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &MockTerminal{toSend: []byte(tc.keys)}
			term := NewTerminal(c, "> ")
			term.SetSize(tc.width, 24)
			for {
				if _, err := term.ReadLine(); err != nil {
					break
				}
			}
			s := &screen{w: tc.width, rows: map[int][]rune{}}
			s.play(string(c.received))
			if s.x != tc.x || s.y != tc.y {
				t.Errorf("cursor at (%d, %d), want (%d, %d); output %q", s.x, s.y, tc.x, tc.y, c.received)
			}
			for i, want := range tc.rows {
				if got := s.row(i); got != want {
					t.Errorf("row %d = %q, want %q", i, got, want)
				}
			}
		})
	}
}
