// Package fsstamp reads a file's size and modification time, the stamp the
// engine compares on every statement to notice another connection's commit.
//
// It exists for js/wasm, where os.Stat is an asynchronous host call routed
// through syscall/js and costs tens of microseconds -- most of a point
// query. There the host answers through one synchronous import instead
// (examples/wasm/musql.js). On Linux and macOS it is unix.Stat into a stack
// Stat_t, and elsewhere os.Stat.
package fsstamp
