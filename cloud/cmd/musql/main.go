// musql is the command line for musql cloud: sign in, manage organizations,
// groups and databases, and run SQL over Hrana.
//
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

	"golang.org/x/term"

	"github.com/samyfodil/musql/cloud/cli"
)

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
		Stdin:            os.Stdin,
		Stdout:           os.Stdout,
		Stderr:           os.Stderr,
		Interactive:      term.IsTerminal(fd),
		StderrIsTerminal: term.IsTerminal(int(os.Stderr.Fd())),
		StdinFd:          fd,
		OpenBrowser:      openBrowser,
	}, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "musql:", err)
		stop()
		os.Exit(1)
	}
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
