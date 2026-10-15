package fsio

import (
	"os"
	"time"
)

// Rename is os.Rename for the rename that publishes a rewritten database over
// its old file. On Windows a file that ANY handle has open without
// FILE_SHARE_DELETE cannot be renamed over -- and os.Open, os.ReadFile and
// os.Stat's fallbacks open it so, briefly, whenever another connection in the
// process reads the file or stamps it. That is a moment, not a state: the
// rename fails with "Access is denied" only when it lands inside one, which
// failed the replication suite's DDL tests now and then. So the rename is
// retried while the failure is one of those (renameRetryable), for at most
// renameRetryFor; elsewhere renameRetryable is never true and this is
// os.Rename.
func Rename(from, to string) error {
	return renameWith(os.Rename, from, to, renameRetryFor, time.Sleep)
}

// renameRetryFor bounds the retrying: a handle that stays open longer is not a
// passing read, and its error is the answer.
const renameRetryFor = 2 * time.Second

func renameWith(rename func(string, string) error, from, to string, limit time.Duration, sleep func(time.Duration)) error {
	wait, waited := time.Millisecond, time.Duration(0)
	for {
		err := rename(from, to)
		if err == nil || !renameRetryable(err) || waited >= limit {
			return err
		}
		sleep(wait)
		waited += wait
		wait = min(2*wait, 50*time.Millisecond)
	}
}

// renameRetryable is platformRenameRetryable, a variable so a test can run the
// retry loop on a platform whose renames never need it.
var renameRetryable = platformRenameRetryable
