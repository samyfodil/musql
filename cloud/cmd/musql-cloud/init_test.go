package main

import (
	"strings"
	"testing"
)

func TestReadPasswordLine(t *testing.T) {
	for in, want := range map[string]string{"pw\n": "pw", "pw": "pw", "a b\r\nrest": "a b"} {
		if got, err := readPasswordLine(strings.NewReader(in)); err != nil || got != want {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "\n"} {
		if _, err := readPasswordLine(strings.NewReader(in)); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}
