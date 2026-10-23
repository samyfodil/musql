package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

func (a *app) dbCmd() *cobra.Command {
	var listGroup string
	list := a.cmd("list", "List databases", cobra.NoArgs, func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		r, err := a.services.db.List(ctx, connect.NewRequest(&v1.ListDatabasesRequest{Org: org, Group: listGroup}))
		if err != nil {
			return err
		}
		var rows [][]string
		for _, d := range r.Msg.Databases {
			rows = append(rows, []string{d.Name, d.Group, d.Url})
		}
		return a.list(r.Msg, []string{"NAME", "GROUP", "URL"}, rows)
	})
	list.Flags().StringVar(&listGroup, "group", "", "only this group's databases")

	var c struct {
		group, fromFile, fromDB, timestamp string
	}
	create := a.cmd("create <name>", "Create a database", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		req := &v1.CreateDatabaseRequest{Org: org, Name: args[0], Group: c.group}
		switch {
		case c.fromFile != "" && c.fromDB != "":
			return errors.New("--from-file and --from-db cannot be combined")
		case c.fromFile != "":
			up, err := a.upload(ctx, org, c.fromFile)
			if err != nil {
				return err
			}
			req.Source = &v1.CreateDatabaseRequest_Upload{Upload: up}
		case c.fromDB != "":
			req.Source = &v1.CreateDatabaseRequest_BranchOf{BranchOf: c.fromDB}
		}
		if req.BranchAt, err = parseTime(c.timestamp); err != nil {
			return err
		}
		if req.BranchAt != nil && c.fromDB == "" {
			return errors.New("--timestamp needs --from-db")
		}
		return a.created(ctx, req)
	})
	create.Flags().StringVar(&c.group, "group", "", "group (default: default)")
	create.Flags().StringVar(&c.fromFile, "from-file", "", "a .musq or C SQLite file to start from")
	create.Flags().StringVar(&c.fromDB, "from-db", "", "branch from this database")
	create.Flags().StringVar(&c.timestamp, "timestamp", "", "with --from-db: the parent as of this time")

	var importName string
	imp := a.cmd("import <file>", "Create a database from a .musq or C SQLite file", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		name := importName
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(args[0]), filepath.Ext(args[0]))
		}
		up, err := a.upload(ctx, org, args[0])
		if err != nil {
			return err
		}
		return a.created(ctx, &v1.CreateDatabaseRequest{Org: org, Name: name, Source: &v1.CreateDatabaseRequest_Upload{Upload: up}})
	})
	imp.Flags().StringVar(&importName, "name", "", "database name (default: the file's name)")

	var urlOnly bool
	show := a.cmd("show <name>", "Show a database", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		d, err := a.get(ctx, args[0])
		if err != nil {
			return err
		}
		switch {
		case urlOnly:
			a.printf("%s\n", d.Url)
		case a.json:
			return writeJSON(a.env.Stdout, d)
		default:
			a.printDB(d)
		}
		return nil
	})
	show.Flags().BoolVar(&urlOnly, "url", false, "print only the URL")

	var yes bool
	destroy := a.cmd("destroy <name>", "Delete a database", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		if !a.confirm(yes, "Delete database "+args[0]+"? This cannot be undone.") {
			return errors.New("not deleted")
		}
		if _, err := a.services.db.Delete(ctx, connect.NewRequest(&v1.DeleteDatabaseRequest{Org: org, Name: args[0]})); err != nil {
			return err
		}
		a.printf("Deleted %s\n", args[0])
		return nil
	})
	destroy.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")

	rename := a.cmd("rename <name> <new-name>", "Rename a database", cobra.ExactArgs(2), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		_, err = a.services.db.Rename(ctx, connect.NewRequest(&v1.RenameDatabaseRequest{Org: org, Name: args[0], NewName: args[1]}))
		return err
	})

	var t struct {
		readOnly   bool
		expiration string
		group      bool
	}
	tokCreate := a.cmd("create <name>", "Create a database token", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		req := &v1.CreateTokenRequest{Org: org, Access: v1.Access_ACCESS_FULL}
		if t.group {
			req.Group = args[0]
		} else {
			req.Name = args[0]
		}
		if t.readOnly {
			req.Access = v1.Access_ACCESS_READ_ONLY
		}
		if req.ExpiresAt, err = parseExpiration(t.expiration, time.Now()); err != nil {
			return err
		}
		r, err := a.services.db.CreateToken(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		a.printf("%s\n", r.Msg.Token)
		return nil
	})
	tokCreate.Flags().BoolVarP(&t.readOnly, "read-only", "r", false, "a read-only token")
	tokCreate.Flags().StringVarP(&t.expiration, "expiration", "e", "never", "lifetime: never, 7d or a duration like 12h")
	tokCreate.Flags().BoolVar(&t.group, "group", false, "the name is a group: the token opens all of its databases")
	var invYes, invGroup bool
	tokInvalidate := a.cmd("invalidate <name>", "Invalidate every token of a database", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		if !a.confirm(invYes, "Invalidate all tokens of "+args[0]+"?") {
			return errors.New("nothing invalidated")
		}
		req := &v1.InvalidateTokensRequest{Org: org, Name: args[0]}
		if invGroup {
			req.Name, req.Group = "", args[0]
		}
		_, err = a.services.db.InvalidateTokens(ctx, connect.NewRequest(req))
		return err
	})
	tokInvalidate.Flags().BoolVarP(&invYes, "yes", "y", false, "do not ask")
	tokInvalidate.Flags().BoolVar(&invGroup, "group", false, "the name is a group")

	var e struct {
		sqlite bool
		output string
	}
	export := a.cmd("export <name>", "Download a database file", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		format, ext := v1.FileFormat_FILE_FORMAT_MUSQL, ".musq"
		if e.sqlite {
			format, ext = v1.FileFormat_FILE_FORMAT_SQLITE, ".sqlite"
		}
		out := e.output
		if out == "" {
			out = args[0] + ext
		}
		r, err := a.services.db.Export(ctx, connect.NewRequest(&v1.ExportRequest{Org: org, Name: args[0], Format: format}))
		if err != nil {
			return err
		}
		if err := os.WriteFile(out, r.Msg.Content, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(a.env.Stderr, "Wrote %s (%s)\n", out, humanBytes(int64(len(r.Msg.Content))))
		return nil
	})
	export.Flags().BoolVar(&e.sqlite, "sqlite", false, "a C SQLite file instead of .musq")
	export.Flags().StringVarP(&e.output, "output", "o", "", "file to write (default: <name>.musq or .sqlite)")

	var r struct{ timestamp, newName string }
	restore := a.cmd("restore <name>", "Restore a database to a point in time", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		at, err := parseTime(r.timestamp)
		if err != nil {
			return err
		}
		if at == nil {
			return errors.New("--timestamp is required")
		}
		res, err := a.services.db.Restore(ctx, connect.NewRequest(&v1.RestoreRequest{Org: org, Name: args[0], At: at, NewName: r.newName}))
		if err != nil {
			return err
		}
		a.printf("Restored into %s\n", res.Msg.Database.Name)
		return nil
	})
	restore.Flags().StringVar(&r.timestamp, "timestamp", "", "the time to restore to (RFC 3339)")
	restore.Flags().StringVar(&r.newName, "new-name", "", "restore into a new database (default: in place)")

	var branchAt string
	branch := a.cmd("branch <parent> <name>", "Create a database as a branch of another", cobra.ExactArgs(2), func(ctx context.Context, args []string) error {
		org, err := a.orgSlug(ctx)
		if err != nil {
			return err
		}
		req := &v1.CreateDatabaseRequest{Org: org, Name: args[1], Source: &v1.CreateDatabaseRequest_BranchOf{BranchOf: args[0]}}
		if req.BranchAt, err = parseTime(branchAt); err != nil {
			return err
		}
		return a.created(ctx, req)
	})
	branch.Flags().StringVar(&branchAt, "timestamp", "", "branch the parent as of this time")

	inspect := a.cmd("inspect <name>", "Show a database's size and usage", cobra.ExactArgs(1), func(ctx context.Context, args []string) error {
		d, err := a.get(ctx, args[0])
		if err != nil {
			return err
		}
		u, err := a.services.db.Usage(ctx, connect.NewRequest(&v1.UsageRequest{Org: d.Org, Name: d.Name,
			From: timestamppb.New(time.Now().Add(-24 * time.Hour)), To: timestamppb.Now()}))
		unsupported := connect.CodeOf(err) == connect.CodeUnimplemented
		if err != nil && !unsupported {
			return err
		}
		if a.json {
			if unsupported {
				return writeJSON(a.env.Stdout, d)
			}
			return writeJSON(a.env.Stdout, u.Msg)
		}
		a.printf("size:  %s\n", humanBytes(d.SizeBytes))
		if unsupported {
			a.printf("usage: this server does not record usage yet\n")
			return nil
		}
		var read, written, stmts int64
		for _, p := range u.Msg.Points {
			read += p.RowsRead
			written += p.RowsWritten
			stmts += p.Statements
		}
		a.printf("last 24h: %d statements, %d rows read, %d rows written\n", stmts, read, written)
		return nil
	})

	shell := &cobra.Command{
		Use:   "shell <name> [sql]",
		Short: "Run SQL on a database, interactively or once",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.clients(); err != nil {
				return err
			}
			return a.shell(cmd.Context(), args)
		},
	}

	return group("db", "Manage databases", list, create, show, destroy, rename,
		group("tokens", "Manage database tokens", tokCreate, tokInvalidate),
		imp, export, restore, branch, inspect, shell)
}

func (a *app) get(ctx context.Context, name string) (*v1.Database, error) {
	org, err := a.orgSlug(ctx)
	if err != nil {
		return nil, err
	}
	r, err := a.services.db.Get(ctx, connect.NewRequest(&v1.GetDatabaseRequest{Org: org, Name: name}))
	if err != nil {
		return nil, err
	}
	return r.Msg.Database, nil
}

func (a *app) upload(ctx context.Context, org, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	r, err := a.services.db.Upload(ctx, connect.NewRequest(&v1.UploadRequest{Org: org, Content: b}))
	if err != nil {
		return "", err
	}
	return r.Msg.Upload, nil
}

func (a *app) created(ctx context.Context, req *v1.CreateDatabaseRequest) error {
	r, err := a.services.db.Create(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	if a.json {
		return writeJSON(a.env.Stdout, r.Msg.Database)
	}
	a.printf("Created database %s at %s\n", r.Msg.Database.Name, r.Msg.Database.Url)
	return nil
}

func (a *app) printDB(d *v1.Database) {
	rows := [][2]string{
		{"Name", d.Name}, {"ID", d.Id}, {"Org", d.Org}, {"Group", d.Group},
		{"URL", d.Url}, {"Size", humanBytes(d.SizeBytes)}, {"Created", when(d.CreatedAt)},
	}
	if d.Parent != "" {
		rows = append(rows, [2]string{"Parent", d.Parent}, [2]string{"Parent at", when(d.ParentAt)})
	}
	for _, r := range rows {
		a.printf("%-10s %s\n", r[0]+":", r[1])
	}
}
