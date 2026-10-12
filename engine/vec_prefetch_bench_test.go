package engine

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// BenchmarkVecPrefetch reads 250 vectors of 384 components, the texts of one
// 250-row INSERT, ahead on vecWorkers goroutines and then takes each.
func BenchmarkVecPrefetch(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	var texts [][]byte
	for i := 0; i < 250; i++ {
		var sb strings.Builder
		sb.WriteString("[")
		for j := 0; j < 384; j++ {
			if j > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "%.6g", rng.NormFloat64()*float64(1+rng.Intn(3)))
		}
		sb.WriteString("]")
		texts = append(texts, []byte(sb.String()))
	}
	for _, workers := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			saved := vecWorkers
			vecWorkers = workers
			defer func() { vecWorkers = saved }()
			for b.Loop() {
				vecPrefetch(texts, vecF32)
				for _, s := range texts {
					if _, err := parseVectorText(s, vecF32); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
