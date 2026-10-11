/** The first character of a name for an avatar tile: a whole code point, so a Thai consonant or an emoji is never cut. */
export function initialOf(name: string): string {
  return Array.from(name.trim())[0]?.toUpperCase() ?? '?';
}

/** `text` cut to `max` runes: a longer one ends in `…` in place of its last rune, without a space before it. */
export function clipRunes(text: string, max: number): string {
  const runes = Array.from(text);
  return runes.length > max ? `${runes.slice(0, max - 1).join('').trimEnd()}…` : text;
}
