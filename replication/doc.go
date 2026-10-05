// Package replication replicates a musql database across nodes. Modes: CRDT
// (every node writes, eventually consistent) or Leader (one writer). Networking
// and consensus are caller-supplied. Changes are captured at commit with
// last-writer-wins clocks and exchanged by version vectors.
package replication
