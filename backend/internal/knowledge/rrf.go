package knowledge

import (
	"bytes"
	"cmp"
	"slices"

	"github.com/google/uuid"
)

// RRF fuses ranked lists of hits by reciprocal rank fusion: a chunk scores the sum, over
// the lists that hold it, of 1/(k+rank) with rank counted from 1, and the result is
// every chunk once, best first, with that sum as its Score. Equal scores are ordered by
// chunk id, so the same lists always fuse to the same order. A chunk repeated within one
// list counts once, at its best rank; the lists themselves are left as they were.
func RRF(lists [][]Hit, k int) []Hit {
	type fused struct {
		hit   Hit
		score float64
	}
	byID := make(map[uuid.UUID]*fused)
	for _, list := range lists {
		seen := make(map[uuid.UUID]bool, len(list))
		for rank, h := range list {
			if seen[h.ChunkID] {
				continue
			}
			seen[h.ChunkID] = true
			f := byID[h.ChunkID]
			if f == nil {
				f = &fused{hit: h}
				byID[h.ChunkID] = f
			}
			f.score += 1 / float64(k+rank+1)
		}
	}
	out := make([]Hit, 0, len(byID))
	for _, f := range byID {
		f.hit.Score = f.score
		out = append(out, f.hit)
	}
	slices.SortFunc(out, func(a, b Hit) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		return bytes.Compare(a.ChunkID[:], b.ChunkID[:])
	})
	return out
}
