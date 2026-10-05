package engine

import (
	"testing"

	"github.com/samyfodil/musql/internal/jit"
)

func TestConfigureJIT(t *testing.T) {
	defer Configure()
	Configure(WithoutJIT())
	if JITEnabled() || vmJITEnabled {
		t.Fatal("WithoutJIT left a JIT on")
	}
	Configure()
	if JITEnabled() != jit.Available || vmJITEnabled != jit.Available {
		t.Fatalf("defaults: jit=%v vm=%v, want %v", JITEnabled(), vmJITEnabled, jit.Available)
	}
}
