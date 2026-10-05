// doomhl runs Doom headless on musql's VDBE, reporting FPS and writing the final frame.
package main

import (
	"flag"
	"fmt"
	"hash/fnv"
	"image"
	"image/png"
	"log"
	"os"
	"runtime/pprof"
	"time"

	"github.com/samyfodil/musql/engine"
	"github.com/samyfodil/musql/examples/doom/assets"
	"github.com/samyfodil/musql/examples/doom/vdbecc"
)

func main() {
	llDir := flag.String("ll", "", "directory of Doom's .ll modules (default: download)")
	wad := flag.String("wad", "", "DOOM1.WAD (default: download)")
	ticks := flag.Int64("ticks", 200, "game ticks to run")
	out := flag.String("png", "frame.png", "where to write the last frame")
	prof := flag.String("cpuprofile", "", "write a CPU profile of the run")
	useJIT := flag.Bool("jit", true, "run generated machine code")
	flag.Parse()
	if *llDir == "" || *wad == "" {
		ll, w, err := assets.Fetch()
		if err != nil {
			log.Fatal(err)
		}
		if *llDir == "" {
			*llDir = ll
		}
		if *wad == "" {
			*wad = w
		}
	}
	if !*useJIT {
		engine.Configure(engine.WithoutJIT())
	}

	t0 := time.Now()
	c, err := vdbecc.LoadDoom(*llDir, *wad, *ticks)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("compiled: %d instructions, %d registers in %v\n", len(c.Program.Insns), c.Program.NReg, time.Since(t0))

	st, err := engine.NewProgramStmt(c.Program)
	if err != nil {
		log.Fatal(err)
	}
	if *prof != "" {
		f, err := os.Create(*prof)
		if err != nil {
			log.Fatal(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal(err)
		}
		defer pprof.StopCPUProfile()
	}
	var last []byte
	h := fnv.New64a()
	frames := 0
	t1 := time.Now()
	for {
		row, err := st.Step()
		if err != nil {
			log.Fatalf("frame %d: %v", frames, err)
		}
		if row == nil {
			break
		}
		last = row[1].S
		h.Write(last)
		frames++
	}
	el := time.Since(t1)
	fmt.Printf("%d frames in %v: %.1f fps, jit=%v, frames hash %016x\n", frames, el, float64(frames)/el.Seconds(), engine.JITEnabled(), h.Sum64())
	if err := writePNG(*out, last); err != nil {
		log.Fatal(err)
	}
}

func writePNG(path string, fb []byte) error {
	img := image.NewRGBA(image.Rect(0, 0, vdbecc.DoomW, vdbecc.DoomH))
	vdbecc.FrameToRGBA(img.Pix, fb)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
