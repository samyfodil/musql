// Command jitscore prints the JIT lowering score from two runs of the corpus
// in a jitscore build -- one with the JIT, one without (scripts/jit-score):
//
//	go run ./jitscore <dir of the JIT run> <dir of the no-JIT run>
//
// A statement is REACHED when the JIT served any of it: native code entered,
// a kernel or compiled program called, or fewer instructions interpreted than
// without the JIT. Its DEPTH is the share of its work lowered: 1 - (VDBE
// instructions interpreted with the JIT) / (without it). The static share is
// the fraction of the instructions of the programs it ran that have native
// code. The worklist is the opcodes still interpreted with the JIT on,
// ranked by how many times they ran.
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type rec struct {
	kind                           string
	steps, enters, calls, nat, all int64
	ops                            map[string]int64
}

func load(dir string) (map[string]rec, error) {
	files, err := filepath.Glob(filepath.Join(dir, "score-*.tsv"))
	if err != nil {
		return nil, err
	}
	out := map[string]rec{}
	seen := map[string]int{}
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			p := strings.Split(sc.Text(), "\t")
			if len(p) < 8 {
				continue
			}
			n := func(i int) int64 { v, _ := strconv.ParseInt(p[i], 10, 64); return v }
			r := rec{kind: p[1], steps: n(2), enters: n(3), calls: n(4), nat: n(5), all: n(6), ops: map[string]int64{}}
			for _, kv := range strings.Split(p[7], ",") {
				if op, c, ok := strings.Cut(kv, "="); ok {
					v, _ := strconv.ParseInt(c, 10, 64)
					r.ops[op] = v
				}
			}
			// A statement the harness ran more than once keeps each run under
			// its own key, in order, so the two runs pair up run for run.
			key := p[0]
			seen[key]++
			if seen[key] > 1 {
				key += "#" + strconv.Itoa(seen[key])
			}
			out[key] = r
		}
		fh.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: jitscore <jit run dir> <no-jit run dir>")
		os.Exit(2)
	}
	on, err := load(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	off, err := load(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	type kindTally struct {
		n, reached int
		depthSum   float64
	}
	kinds := map[string]*kindTally{}
	var depths, statics []float64
	total, reached := 0, 0
	worklist := map[string]int64{}
	var stepsOn, stepsOff int64
	for key, r := range on {
		o, ok := off[key]
		if !ok || o.steps == 0 && r.steps == 0 && r.calls == 0 && r.enters == 0 {
			continue // not in both runs, or no VDBE work at all (a pragma read, say)
		}
		total++
		stepsOn += r.steps
		stepsOff += o.steps
		k := kinds[r.kind]
		if k == nil {
			k = &kindTally{}
			kinds[r.kind] = k
		}
		k.n++
		for op, c := range r.ops {
			worklist[op] += c
		}
		if r.enters == 0 && r.calls == 0 && r.steps >= o.steps {
			continue
		}
		reached++
		k.reached++
		d := 1.0
		if o.steps > 0 {
			d = 1 - float64(r.steps)/float64(o.steps)
		}
		d = min(max(d, 0), 1)
		depths = append(depths, d)
		k.depthSum += d
		if r.all > 0 {
			statics = append(statics, float64(r.nat)/float64(r.all))
		}
	}
	pct := func(a, b int) float64 { return 100 * float64(a) / float64(max(b, 1)) }
	fmt.Printf("JIT LOWERING SCORE over %d statements\n\n", total)
	fmt.Printf("reach: %d statements (%.1f%%) had part of their work served by the JIT\n", reached, pct(reached, total))
	fmt.Printf("instructions interpreted, all statements: %d with the JIT, %d without (%.1f%% lowered)\n\n",
		stepsOn, stepsOff, 100*(1-float64(stepsOn)/float64(max(stepsOff, 1))))
	stats := func(name string, xs []float64) {
		if len(xs) == 0 {
			fmt.Printf("%s: none\n", name)
			return
		}
		sort.Float64s(xs)
		sum := 0.0
		for _, x := range xs {
			sum += x
		}
		fmt.Printf("%s: mean %.1f%%  median %.1f%%  p10 %.1f%%\n", name, 100*sum/float64(len(xs)), 100*xs[len(xs)/2], 100*xs[len(xs)/10])
	}
	stats("depth, dynamic (reached statements)", depths)
	stats("depth, static  (reached statements)", statics)
	buckets := [5]int{}
	for _, d := range depths {
		switch {
		case d >= 1:
			buckets[4]++
		case d >= 0.75:
			buckets[3]++
		case d >= 0.5:
			buckets[2]++
		case d >= 0.25:
			buckets[1]++
		default:
			buckets[0]++
		}
	}
	fmt.Println("\ndynamic depth histogram (reached statements)")
	for i, name := range []string{"0-25%", "25-50%", "50-75%", "75-99%", "100%"} {
		fmt.Printf("  %-7s %7d  %5.1f%%  %s\n", name, buckets[i], pct(buckets[i], len(depths)), strings.Repeat("#", int(pct(buckets[i], len(depths))/2)))
	}
	fmt.Println("\nby statement kind")
	fmt.Printf("  %-8s %8s %8s %7s %11s\n", "kind", "stmts", "reached", "reach", "mean depth")
	var names []string
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		t := kinds[k]
		md := 0.0
		if t.reached > 0 {
			md = 100 * t.depthSum / float64(t.reached)
		}
		fmt.Printf("  %-8s %8d %8d %6.1f%% %10.1f%%\n", k, t.n, t.reached, pct(t.reached, t.n), md)
	}
	type opw struct {
		op string
		n  int64
	}
	var ws []opw
	for op, n := range worklist {
		ws = append(ws, opw{op, n})
	}
	sort.Slice(ws, func(i, j int) bool { return ws[i].n > ws[j].n })
	fmt.Println("\nworklist: the opcodes still interpreted with the JIT on, by instructions executed")
	for i, w := range ws {
		if i == 20 {
			break
		}
		fmt.Printf("  %2d. %-22s %12d  %5.1f%%\n", i+1, w.op, w.n, 100*float64(w.n)/float64(max(stepsOn, 1)))
	}
}
