// doom plays Doom in a window, executed as musql VDBE bytecode. The program
// runs on its own goroutine; each result row is a frame, and key events are
// bound as parameters while it is paused there.
package main

import (
	"flag"
	"log"
	"sync"

	"github.com/gogpu/gogpu"
	"github.com/gogpu/gogpu/gmath"
	"github.com/gogpu/gogpu/input"
	"github.com/gogpu/gputypes"
	"github.com/samyfodil/musql/engine"
	"github.com/samyfodil/musql/examples/doom/assets"
	"github.com/samyfodil/musql/examples/doom/vdbecc"
)

// keys maps window keys to doomkeys.h codes, with doomgeneric's usual
// bindings: ctrl fires, space uses, shift runs, alt strafes.
var keys = map[input.Key]int64{
	input.KeyUp: 0xad, input.KeyDown: 0xaf, input.KeyLeft: 0xac, input.KeyRight: 0xae,
	input.KeyControlLeft: 0xa3, input.KeyControlRight: 0xa3, input.KeySpace: 0xa2,
	input.KeyShiftLeft: 0x80 + 0x36, input.KeyShiftRight: 0x80 + 0x36,
	input.KeyAltLeft: 0x80 + 0x38, input.KeyAltRight: 0x80 + 0x38,
	input.KeyComma: 0xa0, input.KeyPeriod: 0xa1,
	input.KeyEnter: 13, input.KeyEscape: 27, input.KeyTab: 9, input.KeyBackspace: 0x7f,
	input.KeyY: 'y', input.KeyN: 'n', input.KeyMinus: '-', input.KeyEqual: '=',
	input.Key1: '1', input.Key2: '2', input.Key3: '3', input.Key4: '4',
	input.Key5: '5', input.Key6: '6', input.Key7: '7',
}

func main() {
	llDir := flag.String("ll", "", "directory of Doom's .ll modules (default: download)")
	wad := flag.String("wad", "", "DOOM1.WAD (default: download)")
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

	c, err := vdbecc.LoadDoom(*llDir, *wad, 1<<30)
	if err != nil {
		log.Fatal(err)
	}
	st, err := engine.NewProgramStmt(c.Program)
	if err != nil {
		log.Fatal(err)
	}

	var (
		mu      sync.Mutex
		pending []int64 // key | pressed<<8
		frame   []byte  // latest RGBA frame, nil once drawn
	)
	go func() {
		var seq int64
		for {
			mu.Lock()
			evs := pending
			pending = nil
			mu.Unlock()
			if len(evs) > 0 {
				seq++
				st.Bind(1, engine.Value{Typ: engine.Int, I: seq})
				for i := range 9 {
					v := engine.Value{Typ: engine.Int}
					if i < len(evs) {
						v.I = evs[i] // ponytail: past 9 events in one frame, the rest drop
					}
					st.Bind(i+2, v)
				}
			}
			row, err := st.Step()
			if err != nil {
				log.Fatal(err)
			}
			if row == nil {
				return
			}
			rgba := make([]byte, vdbecc.DoomW*vdbecc.DoomH*4)
			vdbecc.FrameToRGBA(rgba, row[1].S)
			mu.Lock()
			frame = rgba
			mu.Unlock()
		}
	}()

	app := gogpu.NewApp(gogpu.DefaultConfig().
		WithTitle("Doom on musql VDBE").
		WithSize(vdbecc.DoomW*3, vdbecc.DoomH*3).
		WithContinuousRender(true))
	var tex *gogpu.Texture
	app.OnUpdate(func(float64) {
		kb := app.Input().Keyboard()
		mu.Lock()
		defer mu.Unlock()
		for k, code := range keys {
			if kb.JustPressed(k) {
				pending = append(pending, code|1<<8)
			}
			if kb.JustReleased(k) {
				pending = append(pending, code)
			}
		}
	})
	app.OnDraw(func(dc *gogpu.Context) {
		dc.ClearColor(gmath.Hex(0x000000))
		mu.Lock()
		f := frame
		frame = nil
		mu.Unlock()
		if f != nil {
			if tex == nil {
				opts := gogpu.DefaultTextureOptions()
				opts.MagFilter, opts.MinFilter = gputypes.FilterModeNearest, gputypes.FilterModeNearest
				if tex, err = dc.Renderer().NewTextureFromRGBAWithOptions(vdbecc.DoomW, vdbecc.DoomH, f, opts); err != nil {
					log.Fatal(err)
				}
			} else if err := tex.UpdateData(f); err != nil {
				log.Fatal(err)
			}
		}
		if tex != nil {
			w, h := dc.Width(), dc.Height()
			if err := dc.DrawTextureScaled(tex, 0, 0, float32(w), float32(h)); err != nil {
				log.Print(err)
			}
		}
	})
	app.OnClose(func() {
		if tex != nil {
			tex.Destroy()
		}
	})
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
