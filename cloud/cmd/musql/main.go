// musql is the command line for musql: run SQL on a local database file or a
// musqld server, and for musql cloud, sign in, manage organizations, groups
// and databases, and run SQL over Hrana.
//
//	musql open app.musq
//	musql open http://localhost:8080 --auth-token secret
//	musql config set url https://cloud.example.com
//	musql auth login
//	musql db create app
//	musql db shell app
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"

	"golang.org/x/term"

	"github.com/samyfodil/musql/cloud/cli"
)

// version is the release's tag, which the release build sets
// (-ldflags "-X main.version=...").
var version string

func main() {
	dir := os.Getenv("MUSQL_CONFIG_DIR")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "musql:", err)
			os.Exit(1)
		}
		dir = filepath.Join(base, "musql")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fd := int(os.Stdin.Fd())
	err := cli.Run(ctx, cli.Env{
		Dir:              dir,
		Token:            os.Getenv("MUSQL_API_TOKEN"),
		ServerToken:      os.Getenv("MUSQLD_AUTH_TOKEN"),
		Stdin:            os.Stdin,
		Stdout:           os.Stdout,
		Stderr:           os.Stderr,
		Interactive:      term.IsTerminal(fd),
		StderrIsTerminal: term.IsTerminal(int(os.Stderr.Fd())),
		StdinFd:          fd,
		StdoutFd:         int(os.Stdout.Fd()),
		OpenBrowser:      openBrowser,
		Version:          buildVersion(),
	}, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "musql:", err)
		stop()
		os.Exit(1)
	}
}

// buildVersion is version, or for a build without it, the module version go
// install recorded, or else "devel" and the commit it was built from.
func buildVersion() string {
	if version != "" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	v, dirty := "devel", false
	for _, s := range bi.Settings {
		switch {
		case s.Key == "vcs.revision" && len(s.Value) >= 12:
			v += " " + s.Value[:12]
		case s.Key == "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if dirty {
		v += "-dirty"
	}
	return v
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
