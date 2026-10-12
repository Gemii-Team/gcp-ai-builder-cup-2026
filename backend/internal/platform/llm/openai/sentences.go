package openai

import (
	"strings"
	"unicode"
)

const (
	// thaiPauseRunes: inside Thai text a single space is a breath pause, and ends a
	// sentence once the sentence holds this many runes. The ASR writes spaces
	// inconsistently, so this alone does not bound a sentence.
	thaiPauseRunes = 60
	// maxSentenceRunes is the longest sentence SplitSentences returns. Thai has no
	// full stop, and a 30 s Thai chunk comes back as one run-on of 400 characters.
	maxSentenceRunes = 160
)

// SplitSentences splits the text of a transcription into sentences. It breaks after a
// ., ? or ! that is followed by a space or the end (not in 3.5 or v1.1), at every
// line break, and at a single space between two Thai words once the sentence holds
// thaiPauseRunes. A sentence still longer than maxSentenceRunes is cut at the last
// space, or boundary between Thai and Latin script, in its second half (the ASR joins
// the two with no space: "ครับGood morning"), and failing that at the last point that
// does not part a Thai consonant from its vowel or tone mark. Nothing but whitespace is
// dropped; every sentence is trimmed.
func SplitSentences(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		for _, piece := range splitLine([]rune(strings.TrimSpace(line))) {
			out = append(out, capPiece(piece)...)
		}
	}
	return out
}

// splitLine splits one line at full stops and Thai pauses.
func splitLine(rs []rune) [][]rune {
	var pieces [][]rune
	start := 0
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == '.' || r == '?' || r == '!':
			j := i + 1
			for j < len(rs) && isCloser(rs[j]) {
				j++
			}
			if j < len(rs) && (rs[j] == '.' || rs[j] == '?' || rs[j] == '!') {
				continue // a run such as "?!" or "...": its last mark decides
			}
			if j == len(rs) || unicode.IsSpace(rs[j]) {
				pieces = append(pieces, rs[start:j])
				start, i = j, j-1
			}
		case unicode.IsSpace(r):
			if i-start >= thaiPauseRunes && i > 0 && i+1 < len(rs) && isThai(rs[i-1]) && isThai(rs[i+1]) {
				pieces = append(pieces, rs[start:i])
				start = i + 1
			}
		}
	}
	return append(pieces, rs[start:])
}

// capPiece trims a piece and cuts it to at most maxSentenceRunes.
func capPiece(piece []rune) []string {
	var out []string
	emit := func(rs []rune) {
		if s := strings.TrimSpace(string(rs)); s != "" {
			out = append(out, s)
		}
	}
	rs := trimSpace(piece)
	for len(rs) > maxSentenceRunes {
		cut := cutPoint(rs)
		emit(rs[:cut])
		rs = trimSpace(rs[cut:])
	}
	emit(rs)
	return out
}

func trimSpace(rs []rune) []rune {
	for len(rs) > 0 && unicode.IsSpace(rs[0]) {
		rs = rs[1:]
	}
	for len(rs) > 0 && unicode.IsSpace(rs[len(rs)-1]) {
		rs = rs[:len(rs)-1]
	}
	return rs
}

// cutPoint is where to cut rs, which is longer than maxSentenceRunes: the latest space or
// boundary between scripts in its second half, else the latest safe point at all.
func cutPoint(rs []rune) int {
	for p := maxSentenceRunes; p >= maxSentenceRunes/2; p-- {
		if unicode.IsSpace(rs[p]) {
			return p
		}
		if safeCut(rs, p) && scriptOf(rs[p-1]) != 0 && scriptOf(rs[p]) != 0 && scriptOf(rs[p-1]) != scriptOf(rs[p]) {
			return p
		}
	}
	for p := maxSentenceRunes; p > 0; p-- {
		if safeCut(rs, p) {
			return p
		}
	}
	return maxSentenceRunes
}

// safeCut reports whether rs may be cut before rs[p]: not between a Thai consonant and a
// vowel or mark that is written after it or above or below it, not after a vowel that is
// written before its consonant, and not before any other combining mark.
func safeCut(rs []rune, p int) bool {
	return !isThaiDependent(rs[p]) && !isThaiLeadingVowel(rs[p-1]) && !unicode.IsMark(rs[p])
}

// isThaiDependent is a Thai vowel, mark or sign that belongs to the letter before it:
// ฯ ะ ั า ำ ิ ี ึ ื ุ ู ฺ and ๅ ๆ ็ ่ ้ ๊ ๋ ์ ํ ๎.
func isThaiDependent(r rune) bool {
	return r == 0x0E2F || (r >= 0x0E30 && r <= 0x0E3A) || (r >= 0x0E45 && r <= 0x0E4E)
}

// isThaiLeadingVowel is เ แ โ ใ ไ, written before the consonant they follow in speech.
func isThaiLeadingVowel(r rune) bool { return r >= 0x0E40 && r <= 0x0E44 }

func isThai(r rune) bool { return r >= 0x0E00 && r <= 0x0E7F }

// isCloser is a quote or bracket that may follow the full stop it ends a sentence with.
func isCloser(r rune) bool {
	switch r {
	case '"', '\'', ')', ']', '”', '’', '»':
		return true
	}
	return false
}

// scriptOf is 't' for a Thai letter, 'l' for a Latin one, 0 for anything else.
func scriptOf(r rune) byte {
	switch {
	case isThai(r) && unicode.IsLetter(r):
		return 't'
	case unicode.Is(unicode.Latin, r):
		return 'l'
	}
	return 0
}
