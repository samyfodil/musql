package libp2p_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/protocol"

	replp2p "github.com/samyfodil/musql/examples/libp2p"
)

// TestStreamsResetOnGarbage: a request stream or an image stream that sends a
// frame too large to read, or a request naming nothing, is reset without an
// answer.
func TestStreamsResetOnGarbage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hA, hB := newHost(t), newHost(t)
	connectHosts(t, hB, hA)
	ta, err := replp2p.New(ctx, hA, nil, "raw", noImages{})
	if err != nil {
		t.Fatal(err)
	}
	defer ta.Close()
	huge := []byte{0xff, 0xff, 0xff, 0xff, 0x0f}
	empty := []byte{0x00}
	for name, tc := range map[string]struct {
		proto protocol.ID
		frame []byte
	}{
		"oversized request": {replp2p.ProtocolID + "/raw", huge},
		"empty request":     {replp2p.ProtocolID + "/raw", empty},
		"oversized image":   {replp2p.ImageProtocolID + "/raw", huge},
	} {
		s, err := hB.NewStream(ctx, hA.ID(), tc.proto)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := s.Write(tc.frame); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s.CloseWrite()
		if b, err := io.ReadAll(s); err == nil && len(b) > 0 {
			t.Errorf("%s: answered %d bytes", name, len(b))
		}
		s.Close()
	}
}
