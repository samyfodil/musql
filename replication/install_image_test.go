package replication

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"
)

// cannedImage is an Imager answering with one manifest and body, or an error.
type cannedImage struct {
	m    ImageManifest
	body []byte
	err  error
}

func (c cannedImage) GetImage(context.Context, PeerID, ImageRequest) (ImageManifest, io.ReadCloser, error) {
	if c.err != nil {
		return ImageManifest{}, nil, c.err
	}
	return c.m, io.NopCloser(bytes.NewReader(c.body)), nil
}

// TestInstallImageFailures: an install that cannot get an image, is offered
// another layout, a torn body or one whose contents its manifest misnames, or
// finds the node holding data of its own, fails and leaves nothing behind.
func TestInstallImageFailures(t *testing.T) {
	ctx := context.Background()
	quiet := []Option{WithSyncInterval(time.Hour)}
	c := newCluster(t)
	c.openWith("a", quiet, imageDDL...)
	c.exec("a", `INSERT INTO t VALUES(1, 'one')`)
	m, body, err := syncerOf(t, c.dbs["a"]).store.Image(ctx, imageRequest(bookkeepingLayout))
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	c.openWith("b", quiet)
	b := syncerOf(t, c.dbs["b"])
	layout, site := m, m
	layout.Layout, site.Site = "9", "zz"
	for name, im := range map[string]cannedImage{
		"no image":       {err: errors.New("unreachable")},
		"another layout": {m: layout, body: img},
		"a torn body":    {m: m, body: img[:len(img)/2]},
		"a misnamed one": {m: site, body: img},
	} {
		if err := b.installImage(ctx, im, "a"); err == nil {
			t.Errorf("%s: installed", name)
		}
	}
	if l := leftovers(t, filepath.Join(c.dir, "b.db")); len(l) > 0 {
		t.Fatalf("left beside b: %v", l)
	}
	c.exec("b", `CREATE TABLE mine(x)`)
	if err := b.installImage(ctx, cannedImage{m: m, body: img}, "a"); !errors.Is(err, errNotEmpty) {
		t.Fatalf("over a node with data of its own: %v, want errNotEmpty", err)
	}
}
