// Command musql-convert moves a database between the SQLite file format and
// musql's own. It is the one program in this repository that calls the
// converter (convert/sqlite); the engine never reads or writes a SQLite file.
//
//	musql-convert import [-force] <in.db> <out.musq>
//	musql-convert export [-page-size N] <in.musq> <out.db>
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/samyfodil/musql/convert/sqlite"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "import":
		fs := flag.NewFlagSet("import", flag.ExitOnError)
		force := fs.Bool("force", false, "convert a source that fails integrity_check")
		fs.Parse(os.Args[2:])
		if fs.NArg() != 2 {
			usage()
		}
		err = sqlite.Import(fs.Arg(0), fs.Arg(1), sqlite.ImportOptions{Force: *force})
	case "export":
		fs := flag.NewFlagSet("export", flag.ExitOnError)
		pageSize := fs.Int("page-size", 0, "page size of the SQLite file (default: the database's own)")
		fs.Parse(os.Args[2:])
		if fs.NArg() != 2 {
			usage()
		}
		err = sqlite.Export(fs.Arg(0), fs.Arg(1), *pageSize)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "musql-convert:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:\n  musql-convert import [-force] <in.db> <out.musq>\n  musql-convert export [-page-size N] <in.musq> <out.db>")
	os.Exit(2)
}
