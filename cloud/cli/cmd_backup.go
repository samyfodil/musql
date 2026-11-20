package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/samyfodil/musql/cloud/store"
)

// backupCmd copies a local database's history to an S3-compatible bucket,
// once or as it is written. The history is cloud/store's commit log, kept
// beside the database: a restore can go back to any commit it logged.
func (a *app) backupCmd() *cobra.Command {
	var (
		stream                bool
		interval, snapshotAge time.Duration
		historyDir, endpoint  string
	)
	cmd := &cobra.Command{
		Use:   "backup <file> s3://bucket/prefix",
		Short: "Back up a local database to S3-compatible storage, once or as it is written",
		Long: `Back up a local database to S3-compatible storage (AWS S3, MinIO, Cloudflare R2, ...).

The database's commits are logged in a history directory beside it, and the
bucket gets a copy of that history: a snapshot of the database, then every
commit's bytes. musql restore can go back to any commit the copy holds.

With --stream it keeps running, copying each commit within --interval of it,
and takes a new snapshot every --snapshot-every so a restore replays less.

Credentials are $AWS_ACCESS_KEY_ID and $AWS_SECRET_ACCESS_KEY, else the shared
credentials file, else the instance's role. The endpoint is --endpoint, else
$AWS_ENDPOINT_URL, else AWS; the region is $AWS_REGION.`,
		Example: `  musql backup app.musq s3://my-bucket/app
  musql backup app.musq s3://my-bucket/app --stream
  musql backup app.musq s3://backups/app --endpoint http://localhost:9000 --stream`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			db := args[0]
			if _, err := os.Stat(db); err != nil {
				return err
			}
			b, err := a.s3Bucket(args[1], endpoint)
			if err != nil {
				return err
			}
			if historyDir == "" {
				historyDir = db + ".history"
			}
			h, err := store.Open(db, historyDir, store.KeepFile())
			if err != nil {
				return err
			}
			defer h.Close()
			m, err := startMirror(ctx, b, h, historyDir)
			if err != nil {
				return err
			}
			if _, err := m.push(ctx, h); err != nil {
				return err
			}
			if !stream {
				a.printf("Backed up %s to %s.\n", db, b)
				return nil
			}
			fmt.Fprintf(a.env.Stderr, "Streaming %s to %s; interrupt to stop.\n", db, b)
			return a.stream(ctx, h, m, db, interval, snapshotAge)
		},
	}
	cmd.Flags().BoolVar(&stream, "stream", false, "keep running, copying each commit as it lands")
	cmd.Flags().DurationVar(&interval, "interval", time.Second, "with --stream, how often to look for new commits")
	cmd.Flags().DurationVar(&snapshotAge, "snapshot-every", 24*time.Hour, "with --stream, how often to take a new snapshot (0: never)")
	cmd.Flags().StringVar(&historyDir, "history", "", "the history directory (default <file>.history)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "the S3 endpoint URL (default $AWS_ENDPOINT_URL, else AWS)")
	return cmd
}

// stream copies commits until ctx ends, then once more. A failed copy is
// reported and retried at the next tick: what is not copied stays in the
// local history.
func (a *app) stream(ctx context.Context, h *store.History, m *mirror, db string, interval, snapshotAge time.Duration) error {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	lastSnapshot := time.Now()
	var stamp string
	round := func(ctx context.Context) {
		s := fileStamp(db)
		due := snapshotAge > 0 && time.Since(lastSnapshot) >= snapshotAge
		if s == stamp && !due {
			return
		}
		err := h.Sync()
		if err == nil && due {
			if err = h.SnapshotIfChanged(); err == nil {
				lastSnapshot = time.Now()
			}
		}
		if err == nil {
			_, err = m.push(ctx, h)
		}
		if err != nil {
			fmt.Fprintf(a.env.Stderr, "musql: backup: %v (retrying)\n", err)
			return
		}
		stamp = s
	}
	for {
		select {
		case <-ctx.Done():
			// One last copy of what landed since the last tick, past the
			// interrupt that ended the stream.
			round(context.WithoutCancel(ctx))
			return nil
		case <-tick.C:
			round(ctx)
		}
	}
}

// fileStamp changes when the database's files do; an unchanged one has no
// commit to copy.
func fileStamp(db string) string {
	var s string
	for _, p := range []string{db, db + ".delta"} {
		if fi, err := os.Stat(p); err == nil {
			s += fmt.Sprintf("%d.%d;", fi.Size(), fi.ModTime().UnixNano())
		} else {
			s += "-;"
		}
	}
	return s
}

// restoreCmd writes a database back out of a backup.
func (a *app) restoreCmd() *cobra.Command {
	var timestamp, endpoint string
	var list bool
	cmd := &cobra.Command{
		Use:   "restore s3://bucket/prefix [file]",
		Short: "Restore a local database from a backup, as of any commit it holds",
		Example: `  musql restore s3://my-bucket/app app.musq
  musql restore s3://my-bucket/app app.musq --timestamp 2026-11-19T09:30:00Z
  musql restore s3://my-bucket/app --list`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if !list && len(args) != 2 {
				return errors.New("restore needs the file to write, or --list")
			}
			b, err := a.s3Bucket(args[0], endpoint)
			if err != nil {
				return err
			}
			at := time.Now()
			if ts, err := parseTime(timestamp); err != nil {
				return err
			} else if ts != nil {
				at = ts.AsTime()
			}
			work := "."
			if !list {
				work = filepath.Dir(args[1])
				if _, err := os.Stat(args[1]); err == nil {
					return fmt.Errorf("%s exists: restore writes a new file", args[1])
				}
			}
			tmp, err := os.MkdirTemp(work, ".musql-restore-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmp)
			h, files, err := fetch(ctx, b, tmp)
			if err != nil {
				return err
			}
			defer h.Close()
			if list {
				for _, s := range h.Snapshots() {
					a.printf("%s  %d bytes\n", s.At.Local().Format(time.RFC3339), s.Size)
				}
				return nil
			}
			names, err := h.RestoreFiles(at)
			if errors.Is(err, store.ErrBeforeHistory) {
				return fmt.Errorf("%s begins at %s, after %s", b, h.Oldest().Local().Format(time.RFC3339), at.Local().Format(time.RFC3339))
			}
			if err != nil {
				return err
			}
			for _, n := range names {
				if err := b.fetchFile(ctx, files, n, tmp); err != nil {
					return err
				}
			}
			if err := h.Restore(at, args[1]); err != nil {
				return err
			}
			a.printf("Restored %s as of %s to %s.\n", b, at.Local().Format(time.RFC3339), args[1])
			return nil
		},
	}
	cmd.Flags().StringVar(&timestamp, "timestamp", "", "the time to restore to (RFC 3339; default: the latest commit)")
	cmd.Flags().BoolVar(&list, "list", false, "list the backup's snapshots instead")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "the S3 endpoint URL (default $AWS_ENDPOINT_URL, else AWS)")
	return cmd
}
