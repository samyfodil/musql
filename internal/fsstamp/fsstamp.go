// Package fsstamp reads a file's size and modification time, the stamp the
// engine compares on every statement to notice another connection's commit.
//
// It exists for js/wasm, where os.Stat is an asynchronous host call routed
// through syscall/js and costs tens of microseconds -- most of a point
// query. There the host answers through one synchronous import instead
// (examples/wasm/musql.js). Everywhere else it is os.Stat.
package fsstamp
