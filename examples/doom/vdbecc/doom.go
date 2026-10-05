package vdbecc

import (
	"fmt"
	"os"
	"path/filepath"
)

// Doom's screen, as doomgeneric's vdbe port presents it: 320x200 XRGB8888.
const (
	DoomW = 320
	DoomH = 200
)

// LoadDoom compiles doomgeneric's vdbe port: every .ll in llDir, linked, with
// the WAD patched into @vdbe_wad. The program runs ticks game ticks and
// returns one [ret, frame] row per drawn frame. Input is ?1 (a sequence
// number; a new one means new events) and ?2..?10 (key | pressed<<8, Doom key
// codes, 0 ends the list), bound while the program is paused at a row.
func LoadDoom(llDir, wadPath string, ticks int64) (*Compiled, error) {
	files, err := filepath.Glob(filepath.Join(llDir, "*.ll"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("vdbecc: no .ll files in %s", llDir)
	}
	mods := make([]*Module, len(files))
	for i, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if mods[i], err = ParseModule(string(src)); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
	}
	m, err := LinkModules(mods)
	if err != nil {
		return nil, err
	}
	wad, err := os.ReadFile(wadPath)
	if err != nil {
		return nil, err
	}
	return Compile(m, Options{
		Memory:  32 << 20,
		Entry:   "vdbe_run",
		Args:    []int64{ticks},
		Frame:   "vdbe_fb",
		Preload: map[string][]byte{"vdbe_wad": wad},
	})
}

// FrameToRGBA converts an XRGB8888 (little-endian B,G,R,X) frame to RGBA.
func FrameToRGBA(dst, fb []byte) {
	for i := 0; i+3 < len(fb) && i+3 < len(dst); i += 4 {
		dst[i], dst[i+1], dst[i+2], dst[i+3] = fb[i+2], fb[i+1], fb[i], 0xff
	}
}
