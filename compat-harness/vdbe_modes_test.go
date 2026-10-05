package compat

// engineModes is the list of execution configurations these gates run each
// case under. The bytecode VDBE is musql's only executor -- every statement,
// read or write, compiles to a program or hard-errors (AGENTS.md Rule 1) --
// so there is exactly one. It stays a list so the mode-labelled loops below
// keep their shape if a second configuration ever earns its place.
var engineModes = []string{"vdbe"}
