package knowledge

// DetectLang is the language a question is asked in: "th" when any rune of q is Thai
// (U+0E00-U+0E7F), otherwise "en". One Thai word makes the question Thai, so a
// question that mixes the two scripts is answered in Thai, and one with no letters at
// all (an emoji, a number, nothing) in English, the default.
func DetectLang(q string) string {
	for _, r := range q {
		if isThai(r) {
			return "th"
		}
	}
	return "en"
}
