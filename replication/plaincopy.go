package replication

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

// PlainCopy writes dst, an ordinary copy of the replicated database at src: its
// schema and rows without the bookkeeping tables or the capture guard, so any
// connection may write it and Open makes its rows a new site's first ops. src
// is read in one snapshot and may be open; dst must not exist. Withheld rows
// are not copied.
func PlainCopy(ctx context.Context, src, dst string) (err error) {
	if _, err := os.Stat(src); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("replication: %s exists", dst)
	}
	// VACUUM INTO only reads src, but a guarded file refuses it to any
	// connection that neither captures nor is a bookkeeping one.
	from, err := openApplyDB(src)
	if err != nil {
		return err
	}
	_, err = from.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(dst, "'", "''")+`'`)
	from.Close()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			engine.RemoveDatabaseFiles(dst)
		}
	}()
	to, err := openApplyDB(dst)
	if err != nil {
		return err
	}
	defer to.Close()
	conn, err := to.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Raw(func(dc any) error { return dc.(*driver.Conn).SetCaptureGuard("") }); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, b := range bookkeeping {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS main.`+b); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
