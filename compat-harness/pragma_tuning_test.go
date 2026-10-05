// This file gates the tuning pragmas (synchronous, cache_size,
// journal_size_limit, mmap_size, default_cache_size) and their parsing,
// masking, result shapes, and per-database scoping rules against C SQLite.
package compat

import (
	"fmt"
	"testing"
)

// tuningNames are the pragmas that carry a value.
var tuningNames = []string{"synchronous", "cache_size", "journal_size_limit", "mmap_size"}

// tuningValues covers parsing branches, overflow behaviors, sign forms, and trailing junk.
var tuningValues = []string{
	"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "99",
	"-1", "-2", "-2147483648", "-2147483649", "2147483647", "2147483648",
	"4294967296", "99999999999999999999", "-99999999999999999999",
	"off", "on", "no", "yes", "true", "false", "normal", "full", "extra",
	"memory", "file", "default", "bogus", "delete",
	"0x10", "0x7fffffff", "0xffffffff", "+5", "05", "1.9", "1e5", "-0",
	"'12abc'", "'  7'", "''", "'2'", "'off'",
}

// TestPragmaTuningValues sets and reads each pragma value on a fresh database per case.
func TestPragmaTuningValues(t *testing.T) {
	for _, name := range tuningNames {
		for _, v := range tuningValues {
			set := fmt.Sprintf("PRAGMA %s=%s", name, v)
			get := "PRAGMA " + name
			differ(t, set+" ; "+get, []string{set, get})
		}
	}
}

// TestPragmaTuningDefaults checks default values match the oracle.
func TestPragmaTuningDefaults(t *testing.T) {
	for _, name := range tuningNames {
		differ(t, "PRAGMA "+name, []string{"PRAGMA " + name})
		differ(t, "PRAGMA main."+name, []string{"PRAGMA main." + name})
		differ(t, "PRAGMA temp."+name, []string{"PRAGMA temp." + name})
	}
}

// TestPragmaTuningSetterShape checks which pragmas echo their values when set.
func TestPragmaTuningSetterShape(t *testing.T) {
	for _, name := range tuningNames {
		for _, v := range []string{"0", "1", "4096", "-1", "99999"} {
			set := fmt.Sprintf("PRAGMA %s=%s", name, v)
			differ(t, set, []string{set})
			// the "(value)" spelling is the same setter
			call := fmt.Sprintf("PRAGMA %s(%s)", name, v)
			differ(t, call+" ; get", []string{call, "PRAGMA " + name})
		}
	}
}

// TestPragmaTuningPerDatabase verifies each pragma is per-database, not global.
func TestPragmaTuningPerDatabase(t *testing.T) {
	for _, name := range tuningNames {
		differ(t, "per-database "+name, []string{
			"ATTACH ':memory:' AS aux",
			fmt.Sprintf("PRAGMA aux.%s=7", name),
			"PRAGMA aux." + name,
			"PRAGMA main." + name,
			"PRAGMA " + name,
			fmt.Sprintf("PRAGMA main.%s=3", name),
			"PRAGMA main." + name,
			"PRAGMA aux." + name,
			"PRAGMA temp." + name,
		})
	}
}

// TestPragmaTuningTempSynchronous checks that temp.synchronous is pinned at 0.
func TestPragmaTuningTempSynchronous(t *testing.T) {
	differ(t, "temp.synchronous", []string{
		"PRAGMA temp.synchronous",
		"PRAGMA temp.synchronous=2",
		"PRAGMA temp.synchronous",
		"PRAGMA main.synchronous",
		"CREATE TEMP TABLE tt(x)",
		"PRAGMA temp.synchronous",
	})
}

// TestPragmaTuningValueInTransaction checks synchronous setter behavior in transactions.
func TestPragmaTuningValueInTransaction(t *testing.T) {
	for _, name := range tuningNames {
		differ(t, "in-transaction "+name, []string{
			"CREATE TABLE t(x)",
			"BEGIN",
			fmt.Sprintf("PRAGMA %s=1", name),
			"PRAGMA " + name,
			"COMMIT",
			"PRAGMA " + name,
		})
	}
}

// TestPragmaTuningPersistence verifies tuning values are connection state, not persistent.
func TestPragmaTuningPersistence(t *testing.T) {
	differ(t, "tuning persistence", []string{
		"PRAGMA synchronous=0",
		"PRAGMA cache_size=123",
		"PRAGMA journal_size_limit=4096",
		"PRAGMA mmap_size=65536",
		"CREATE TABLE t(x)",
		"INSERT INTO t VALUES (1),(2),(3)",
		"BEGIN",
		"INSERT INTO t VALUES (4)",
		"COMMIT",
		"PRAGMA synchronous",
		"PRAGMA cache_size",
		"PRAGMA journal_size_limit",
		"PRAGMA mmap_size",
		"SELECT count(*) FROM t",
	})
}

// TestPragmaDefaultCacheSize checks that default_cache_size is not recognized.
func TestPragmaDefaultCacheSize(t *testing.T) {
	for _, q := range []string{
		"PRAGMA default_cache_size",
		"PRAGMA default_cache_size=10",
		"PRAGMA main.default_cache_size",
		"PRAGMA main.default_cache_size=10",
		"PRAGMA temp.default_cache_size",
	} {
		differ(t, q, []string{q})
	}
	// Setting it doesn't affect cache_size.
	differ(t, "default_cache_size is inert", []string{
		"CREATE TABLE t(x)",
		"PRAGMA default_cache_size=10",
		"PRAGMA default_cache_size",
		"PRAGMA cache_size",
	})
}

// TestPragmaAbsentNames checks pragmas that don't exist in the oracle.
func TestPragmaAbsentNames(t *testing.T) {
	for _, name := range []string{
		"default_cache_size", "legacy_file_format", "data_store_directory",
		"omit_readlock",
		"multiplex_enabled", "multiplex_chunksize", "multiplex_filecount",
		"multiplex_truncate",
		"vdbe_listing", "vdbe_trace", "vdbe_addoptrace", "vdbe_debug",
		"parser_trace", "shrink_memory",
	} {
		differ(t, "absent "+name, []string{"PRAGMA " + name})
		differ(t, "absent setter "+name, []string{"PRAGMA " + name + "=1"})
		differ(t, "absent qualified "+name, []string{"PRAGMA main." + name})
	}
}

// TestPragmaTempStoreDirectory checks temp_store_directory accepts and rejects paths correctly.
func TestPragmaTempStoreDirectory(t *testing.T) {
	differ(t, "temp_store_directory lifecycle", []string{
		`PRAGMA temp_store_directory`,
		`PRAGMA temp_store_directory='/tmp/'`,
		`PRAGMA temp_store_directory`,
		`PRAGMA temp_store_directory='/tmp'`,
		`PRAGMA temp_store_directory`,
		`PRAGMA temp_store_directory=''`,
		`PRAGMA temp_store_directory`,
	})
	differ(t, "temp_store_directory rejects", []string{
		`PRAGMA temp_store_directory='/NON/EXISTENT/PATH/FOOBAR'`,
		`PRAGMA temp_store_directory`,
	})
	// Files are not directories; bare syntax is also rejected.
	differ(t, "temp_store_directory not a dir", []string{
		`PRAGMA temp_store_directory='/etc/hostname'`,
		`PRAGMA temp_store_directory`,
	})
	differ(t, "temp_store_directory bare", []string{`PRAGMA temp_store_directory=/tmp`})
	// Reset the oracle's process-global temp directory.
	differ(t, "temp_store_directory reset", []string{`PRAGMA temp_store_directory=''`})
}

// TestPragmaTuningUnknownSchema verifies unknown database qualifiers fail.
func TestPragmaTuningUnknownSchema(t *testing.T) {
	for _, name := range append(tuningNames, "default_cache_size") {
		differ(t, "unknown schema "+name, []string{"PRAGMA nosuchdb." + name})
		differ(t, "unknown schema setter "+name, []string{"PRAGMA nosuchdb." + name + "=1"})
	}
}

// TestPragmaTuningMemoryBacked checks mmap_size behavior on memory-backed databases.
func TestPragmaTuningMemoryBacked(t *testing.T) {
	differ(t, "mmap_size, file-backed vs memory-backed", []string{
		"ATTACH ':memory:' AS m",
		"PRAGMA m.mmap_size",
		"PRAGMA m.mmap_size=65536",
		"PRAGMA m.mmap_size",
		"PRAGMA temp.mmap_size",
		"PRAGMA temp.mmap_size=65536",
		"PRAGMA mmap_size",
		"PRAGMA mmap_size=65536",
		"PRAGMA mmap_size",
		"PRAGMA main.mmap_size",
	})
	// Other pragmas work normally on memory databases.
	for _, name := range []string{"synchronous", "cache_size", "journal_size_limit"} {
		differ(t, "memory-backed "+name, []string{
			"ATTACH ':memory:' AS m",
			"PRAGMA m." + name,
			"PRAGMA m." + name + "=7",
			"PRAGMA m." + name,
			"PRAGMA temp." + name,
		})
	}
}
