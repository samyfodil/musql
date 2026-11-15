package cli

import (
	"database/sql"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tursodatabase/libsql-client-go/libsql"

	_ "github.com/samyfodil/musql/driver"
)

// openCmd runs SQL on a local database file, as db shell does on a cloud one,
// on a musqld server given its URL, or, with no argument or ":memory:", on an
// in-memory database that is gone when it exits.
func (a *app) openCmd() *cobra.Command {
	var token string
	cmd := &cobra.Command{
		Use:   "open [file|url|:memory:] [sql]",
		Short: "Run SQL on a local database file (created if missing), a musqld server, or an in-memory database",
		Example: `  musql open app.musq
  musql open app.musq "SELECT count(*) FROM people;"
  musql open app.musq < schema.sql
  musql open http://localhost:8080 --auth-token secret
  musql open                 # an in-memory database, gone on exit
  musql open :memory: "SELECT 1 + 1;"`,
		Args: cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				args = []string{memoryDB}
			}
			var db *sql.DB
			banner := "Opened " + args[0] + "."
			if args[0] == memoryDB {
				banner = "Opened an in-memory database; nothing is saved when it exits."
			}
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

// memoryDB is the name the driver opens as an in-memory database.
const memoryDB = ":memory:"

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
