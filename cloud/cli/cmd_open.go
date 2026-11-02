package cli

import (
	"database/sql"

	"github.com/spf13/cobra"

	_ "github.com/samyfodil/musql/driver"
)

// openCmd runs SQL on a local database file, as db shell does on a cloud one.
func (a *app) openCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open <file> [sql]",
		Short: "Run SQL on a local database file (created if missing)",
		Example: `  musql open app.musq
  musql open app.musq "SELECT count(*) FROM people;"
  musql open app.musq < schema.sql`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := sql.Open("sqlite", args[0])
			if err != nil {
				return err
			}
			defer db.Close()
			// One connection, so a transaction or a PRAGMA lasts from one
			// statement to the next.
			db.SetMaxOpenConns(1)
			return a.runShell(cmd.Context(), db, args[1:], "Opened "+args[0]+".")
		},
	}
}
