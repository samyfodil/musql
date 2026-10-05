//go:build !sqlite_fts5

// FTS5 testing: this build has fts5 disabled in both engines.
package compat

// harnessFTS5 is false in this build.
const harnessFTS5 = false
