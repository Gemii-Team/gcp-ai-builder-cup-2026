import { describe, expect, it } from 'vitest';
import { clipRunes, initialOf } from './text';

const runes = (text: string) => Array.from(text).length;
/** A UTF-16 surrogate with no partner: what cutting between the two halves of an astral character leaves. */
const LONE_SURROGATE = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;

describe('clipRunes', () => {
  it('leaves a text of 120 runes or fewer as it is', () => {
    expect(clipRunes('Send the quotation to ACME', 120)).toBe('Send the quotation to ACME');
    expect(clipRunes('a'.repeat(120), 120)).toBe('a'.repeat(120));
  });

  it('cuts a longer text to 120 runes in all, the last of them the ellipsis: 119 and then …', () => {
    const clipped = clipRunes('a'.repeat(121), 120);

    expect(clipped).toBe(`${'a'.repeat(119)}…`);
    expect(runes(clipped)).toBe(120);
  });

  it('drops the space before the ellipsis', () => {
    expect(clipRunes(`${'a'.repeat(118)} bbbb`, 120)).toBe(`${'a'.repeat(118)}…`);
  });

  it('counts an astral character as one rune and never splits its surrogate pair', () => {
    const clipped = clipRunes('😀'.repeat(130), 120);

    expect(clipped).toBe(`${'😀'.repeat(119)}…`);
    expect(runes(clipped)).toBe(120);
    expect(LONE_SURROGATE.test(clipped)).toBe(false);
  });

  it('never cuts an astral character that straddles the cut in half, wherever the cut falls', () => {
    // 118 one-unit runes, then emoji: the 119th rune is the first emoji, whose two UTF-16 units straddle unit 119.
    const clipped = clipRunes(`${'a'.repeat(118)}${'😀'.repeat(5)}`, 120);

    expect(LONE_SURROGATE.test(clipped)).toBe(false);
    expect(runes(clipped)).toBe(120);
    expect(clipped).toBe(`${'a'.repeat(118)}😀…`);
  });

  it('keeps a Thai base consonant with its tone and vowel marks: the cut backs off to the cluster before', () => {
    // `น้` is a base and a tone mark, one cluster of two runes; the 119th rune is the base, the 120th its mark.
    const clipped = clipRunes(`${'ก'.repeat(118)}น้ tail of the message`, 120);

    expect(clipped).toBe(`${'ก'.repeat(118)}…`);
    expect(runes(clipped)).toBeLessThanOrEqual(120);
    expect(clipped).not.toMatch(/น…/u);
  });

  it('keeps a cluster that fits whole, with all its marks', () => {
    const clipped = clipRunes(`${'ก'.repeat(117)}น้ำ tail of the message`, 120);

    // น + ้ + ำ is one cluster of three runes: 117 + 3 = 120 > 119, so it does not fit and the text ends before it.
    expect(clipped).toBe(`${'ก'.repeat(117)}…`);
    expect(clipRunes(`${'ก'.repeat(116)}น้ำ tail of the message`, 120)).toBe(`${'ก'.repeat(116)}น้ำ…`);
  });

  it('does not split an emoji sequence that is joined by zero-width joiners', () => {
    const family = '👨‍👩‍👧';
    const clipped = clipRunes(`${'a'.repeat(117)}${family}${family}`, 120);

    expect(clipped).toBe(`${'a'.repeat(117)}…`);
    expect(LONE_SURROGATE.test(clipped)).toBe(false);
  });
});

describe('initialOf', () => {
  it('is the first whole character of the name in upper case', () => {
    expect(initialOf('  nok')).toBe('N');
    expect(initialOf('😀 Team')).toBe('😀');
    expect(initialOf('อาทิตย์')).toBe('อ');
    expect(initialOf('   ')).toBe('?');
  });

  it('keeps a Thai prefix vowel together with the consonant it is written before: เอก, แอน, ไผ่, โต, ใจ', () => {
    expect(initialOf('เอก')).toBe('เอ');
    expect(initialOf('แอน')).toBe('แอ');
    expect(initialOf('ไผ่')).toBe('ไผ');
    expect(initialOf('โต')).toBe('โต');
    expect(initialOf('ใจ')).toBe('ใจ');
    expect(initialOf('  เอก')).toBe('เอ');
  });

  it('keeps a prefix vowel that no consonant follows as it is, and a vowel that is not a prefix vowel is never joined', () => {
    expect(initialOf('เ')).toBe('เ');
    expect(initialOf('เ-')).toBe('เ');
    expect(initialOf('เA')).toBe('เ');
    expect(initialOf('เ๑')).toBe('เ');
    expect(initialOf('อเก')).toBe('อ');
    expect(initialOf('ะก')).toBe('ะ');
  });
});
