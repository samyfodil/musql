//go:build !jitllm

package ai

import "errors"

func newEmbedded(Config) (Generator, error) {
	return nil, errors.New("ai: this build has no embedded jitllm (build with -tags jitllm), or set jitllm.embedded: false and a base_url")
}
