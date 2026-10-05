// Command llcheck parses and links .ll files, as a smoke test of the parser.
package main

import (
	"fmt"
	"os"

	"github.com/samyfodil/musql/examples/doom/vdbecc"
)

func main() {
	var mods []*vdbecc.Module
	for _, f := range os.Args[1:] {
		src, err := os.ReadFile(f)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		m, err := vdbecc.ParseModule(string(src))
		if err != nil {
			fmt.Printf("%s: %v\n", f, err)
			os.Exit(1)
		}
		mods = append(mods, m)
	}
	m, err := vdbecc.LinkModules(mods)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	insts := 0
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			insts += len(b.Body) + 1
		}
	}
	fmt.Printf("%d functions, %d globals, %d instructions, external: %v\n", len(m.Funcs), len(m.Vars), insts, m.External)
}
