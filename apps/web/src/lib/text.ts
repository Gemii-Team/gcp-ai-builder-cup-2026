/** A Thai prefix vowel (เ แ โ ใ ไ) and the consonant it is written before: one letter to read, though the vowel comes first in the text. */
const THAI_PREFIX_VOWEL_AND_CONSONANT = /^[\u0E40-\u0E44][\u0E01-\u0E2E]/u;

/**
 * The first character of a name for an avatar tile: a whole code point, so a Thai consonant or an emoji is never cut; a Thai
 * prefix vowel that a consonant follows keeps it (`เอก` is `เอ`, not a lone vowel).
 */
export function initialOf(name: string): string {
  const trimmed = name.trim();
  return (THAI_PREFIX_VOWEL_AND_CONSONANT.exec(trimmed)?.[0] ?? Array.from(trimmed)[0])?.toUpperCase() ?? '?';
}

const graphemes = new Intl.Segmenter(undefined, { granularity: 'grapheme' });

/**
 * `text` cut to `max` runes in all: a longer one keeps the clusters that fit in `max - 1` runes and ends in `…`, with no
 * space before it. A rune is a code point, never a UTF-16 unit, and the cut falls between clusters, never inside one: an
 * astral character keeps both halves, a Thai consonant keeps its tone and vowel marks, an emoji sequence stays whole.
 */
export function clipRunes(text: string, max: number): string {
  if (Array.from(text).length <= max) {
    return text;
  }
  let kept = '';
  let runes = 0;
  for (const { segment } of graphemes.segment(text)) {
    const size = Array.from(segment).length;
    if (runes + size > max - 1) {
      break;
    }
    kept += segment;
    runes += size;
  }
  return `${kept.trimEnd()}…`;
}
