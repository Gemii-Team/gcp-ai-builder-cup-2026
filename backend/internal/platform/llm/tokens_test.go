package llm_test

import (
	"strings"
	"testing"

	"kiwi/internal/platform/llm"
)

func TestEstimateTokens_ThaiVsLatin(t *testing.T) {
	if got := llm.EstimateTokens(strings.Repeat("ก", 1000)); got != 500 {
		t.Fatalf("1000 Thai runes = %d tokens, want 500", got)
	}
	if got := llm.EstimateTokens(strings.Repeat("a", 1000)); got != 250 {
		t.Fatalf("1000 Latin runes = %d tokens, want 250", got)
	}
}

func TestEstimateTokens_EmptyAndRoundingUp(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"one Latin rune rounds up", "a", 1},
		{"four Latin runes are exactly one", "abcd", 1},
		{"five Latin runes round up", "abcde", 2},
		{"one Thai rune rounds up", "ก", 1},
		{"two Thai runes are exactly one", "กข", 1},
		{"three Thai runes round up", "กขค", 2},
		{"one Thai and one Latin are 0.75", "กa", 1},
		{"two Thai and one Latin are 1.25", "กขa", 2},
		{"multi-byte non-Thai runes count once each", "日本語🙂", 1},
		{"Thai combining marks count as Thai runes", "ก\u0e31\u0e48", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := llm.EstimateTokens(tc.in); got != tc.want {
				t.Fatalf("EstimateTokens(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestEstimateTokens_ThaiBlockBoundaries(t *testing.T) {
	// U+0E00 and U+0E7F are Thai (half a token each); U+0DFF and U+0E80 sit just
	// outside the block (a quarter each), so four runes of each kind differ by a factor 2.
	thai := strings.Repeat("\u0e00", 2)  // 2 * 0.5 = 1
	thai += strings.Repeat("\u0e7f", 2)  // 2 * 0.5 = 1
	other := strings.Repeat("\u0dff", 2) // 2 * 0.25 = 0.5
	other += strings.Repeat("\u0e80", 2) // 2 * 0.25 = 0.5
	if got := llm.EstimateTokens(thai); got != 2 {
		t.Fatalf("U+0E00 and U+0E7F x2 = %d tokens, want 2", got)
	}
	if got := llm.EstimateTokens(other); got != 1 {
		t.Fatalf("U+0DFF and U+0E80 x2 = %d tokens, want 1", got)
	}
}
