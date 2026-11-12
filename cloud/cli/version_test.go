package cli_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/samyfodil/musql/cloud/cli"
)

func TestVersionFlag(t *testing.T) {
	var out bytes.Buffer
	err := cli.Run(context.Background(), cli.Env{Dir: t.TempDir(), Stdout: &out, Stderr: io.Discard, Version: "v9.9.9"}, []string{"--version"})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "musql version v9.9.9\n" {
		t.Errorf("--version printed %q", got)
	}
}
