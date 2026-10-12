package llm

// EstimateTokens estimates how many model tokens s takes without a tokenizer:
// ceil(thaiRunes/2 + otherRunes/4), where a Thai rune is any code point in
// U+0E00-U+0E7F (consonants, vowels, marks and digits) and every other rune,
// including an invalid byte, counts as one. It sizes chunks and prompts, so it errs
// on the side of too many tokens for Thai, which has no spaces and tokenizes finely.
func EstimateTokens(s string) int {
	var thai, other int
	for _, r := range s {
		if r >= 0x0E00 && r <= 0x0E7F {
			thai++
		} else {
			other++
		}
	}
	// thai/2 + other/4 = (2*thai + other)/4, rounded up.
	return (2*thai + other + 3) / 4
}
