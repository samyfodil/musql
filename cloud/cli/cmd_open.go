package cli

import (
	"database/sql"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tursodatabase/libsql-client-go/libsql"

	_ "github.com/samyfodil/musql/driver"
)

// openCmd runs SQL on a local database file, as db shell does on a cloud one,
// or on a musqld server given its URL.
func (a *app) openCmd() *cobra.Command {
	var token string
	cmd := &cobra.Command{
		Use:   "open <file|url> [sql]",
		Short: "Run SQL on a local database file (created if missing) or a musqld server",
		Example: `  musql open app.musq
  musql open app.musq "SELECT count(*) FROM people;"
  musql open app.musq < schema.sql
  musql open http://localhost:8080 --auth-token secret`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var db *sql.DB
			banner := "Opened " + args[0] + "."
			if serverURL(args[0]) {
				conn, err := libsql.NewConnector(args[0], libsql.WithAuthToken(token))
				if err != nil {
					return err
				}
				db, banner = sql.OpenDB(conn), "Connected to "+args[0]+"."
			} else {
				var err error
				if db, err = sql.Open("sqlite", args[0]); err != nil {
					return err
				}
			}
			defer db.Close()
			// One connection, so a transaction or a PRAGMA lasts from one
			// statement to the next.
			db.SetMaxOpenConns(1)
			return a.runShell(cmd.Context(), db, args[1:], banner)
		},
	}
	cmd.Flags().StringVar(&token, "auth-token", a.env.ServerToken, "the musqld server's token, for a URL (default $MUSQLD_AUTH_TOKEN)")
	return cmd
}

// serverURL reports whether open's argument names a server, in a scheme a
// libSQL client speaks, rather than a file.
func serverURL(s string) bool {
	s = strings.ToLower(s)
	for _, p := range []string{"http://", "https://", "ws://", "wss://", "libsql://"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
