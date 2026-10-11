import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

const css = readFileSync(new URL('./theme.css', import.meta.url), 'utf8');

/** Every `--color-<name>: <value>;` declaration of the theme, by token name. */
function colorTokens(): Map<string, string> {
  const tokens = new Map<string, string>();
  for (const match of css.matchAll(/^\s*--color-([a-z-]+):\s*([^;]+);/gm)) {
    tokens.set(match[1], match[2].trim());
  }
  return tokens;
}

function hex(name: string): string {
  const value = colorTokens().get(name);
  if (value === undefined || !/^#[0-9A-Fa-f]{6}$/.test(value)) {
    throw new Error(`--color-${name} is not a 6-digit hex colour in theme.css (got ${String(value)})`);
  }
  return value;
}

/** WCAG 2.x relative luminance of an sRGB hex colour. */
function luminance(color: string): number {
  const [r, g, b] = [1, 3, 5].map((i) => {
    const channel = parseInt(color.slice(i, i + 2), 16) / 255;
    return channel <= 0.03928 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
  }) as [number, number, number];
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(foreground: string, background: string): number {
  const [light, dark] = [luminance(hex(foreground)), luminance(hex(background))].sort((a, b) => b - a) as [
    number,
    number,
  ];
  return (light + 0.05) / (dark + 0.05);
}

// Colours of the prototypes' :root blocks under their DESIGN.md names. A token no prototype uses is not added.
const DESIGN_TOKENS = [
  'primary', 'primary-hover', 'primary-press', 'on-primary',
  'rail-hover', 'rail-press', 'rail-line', 'rail-sage', 'rail-focus', 'rail-selection',
  'presence-green', 'tally-mint', 'doing-teal', 'focus-teal',
  'citation-teal', 'citation-wash', 'citation-hover', 'citation-press', 'mint-press',
  'selection-mint', 'selection-ink', 'counter-mint', 'counter-hover',
  'slip-paper', 'field-white', 'ledger-ink', 'carbon-copy',
  'control-line', 'control-line-hover', 'perforation-sage', 'ledger-rule', 'slip-edge', 'track-rule',
  'skeleton-sage', 'chip-fill', 'chip-edge', 'disabled-fill', 'disabled-ink',
  'seal-green', 'seal-ink', 'seal-wash',
  'draft-amber', 'draft-ink', 'draft-wash', 'draft-tag', 'draft-hover', 'draft-press',
  'returned-red', 'returned-ink', 'returned-wash', 'returned-edge',
  'bill-tan', 'sheet-scrim',
];

describe('theme.css palette', () => {
  it('resets the default Tailwind palette before declaring any colour', () => {
    const reset = css.indexOf('--color-*: initial');
    expect(reset).toBeGreaterThan(-1);
    expect(reset).toBeLessThan(css.indexOf('--color-primary:'));
  });

  it('keeps every token as a CSS variable, because component stylesheets read them with var()', () => {
    // Without `static`, Tailwind drops each variable no utility references, and var(--color-slip-edge) in slip.css
    // would resolve to nothing in the built app.
    expect(css).toMatch(/^@theme static\s*{/m);
  });

  it('declares exactly the DESIGN.md colour tokens', () => {
    expect([...colorTokens().keys()].sort()).toEqual([...DESIGN_TOKENS].sort());
  });

  it('writes every colour as a 6-digit hex, except the translucent sheet scrim', () => {
    for (const [name, value] of colorTokens()) {
      if (name === 'sheet-scrim') {
        expect(value).toBe('rgb(15 61 58 / .45)');
      } else {
        expect(value, `--color-${name}`).toMatch(/^#[0-9A-F]{6}$/);
      }
    }
  });
});

describe('theme.css contrast', () => {
  // Text pairs: WCAG AA for body text. A mistyped hex digit lands here.
  it.each([
    ['ledger-ink', 'slip-paper'],
    ['carbon-copy', 'slip-paper'],
    ['citation-teal', 'citation-wash'],
    ['draft-ink', 'draft-wash'],
    ['returned-ink', 'returned-wash'],
    ['seal-ink', 'seal-wash'],
    ['on-primary', 'primary'],
    ['rail-sage', 'primary'],
    ['disabled-ink', 'disabled-fill'],
  ])('%s on %s is at least 4.5:1', (foreground, background) => {
    expect(contrast(foreground, background)).toBeGreaterThanOrEqual(4.5);
  });

  // A control boundary needs 3:1 against the paper it sits on and against the ground behind it.
  it.each([
    ['control-line', 'slip-paper'],
    ['control-line', 'counter-mint'],
  ])('%s on %s is at least 3:1', (foreground, background) => {
    expect(contrast(foreground, background)).toBeGreaterThanOrEqual(3);
  });
});

describe('theme.css non-colour tokens', () => {
  it('sets Anuphan for language and Noto Sans Mono for what is counted, timed or referenced', () => {
    expect(css).toContain('--font-sans: Anuphan, "Noto Sans Thai", system-ui, sans-serif;');
    expect(css).toContain('--font-mono: "Noto Sans Mono", Anuphan, monospace;');
  });

  it('declares the DESIGN.md radii and the expo ease', () => {
    for (const radius of ['hair: 2px', 'xs: 4px', 'sm: 6px', 'md: 8px', 'tile: 10px', 'lg: 12px', 'xl: 16px', 'notch: 7px', 'pill: 999px']) {
      expect(css).toContain(`--radius-${radius};`);
    }
    expect(css).toContain('--ease-expo: cubic-bezier(.16,1,.3,1);');
  });

  it('declares the DESIGN.md typography roles', () => {
    for (const role of [
      'headline', 'wordmark', 'title', 'item-title', 'body', 'body-phone', 'label', 'button', 'meta',
      'numeral', 'reference', 'citation',
    ]) {
      expect(css, `--text-${role}`).toMatch(new RegExp(`--text-${role}:\\s*[\\d.]+px;`));
      expect(css, `--text-${role}--line-height`).toMatch(new RegExp(`--text-${role}--line-height:`));
    }
    expect(css).toMatch(/--text-headline:\s*20px;\s*--text-headline--line-height:\s*1\.45;/);
    expect(css).toMatch(/--text-body:\s*14\.5px;\s*--text-body--line-height:\s*1\.65;/);
    expect(css).toMatch(/--text-label:\s*14px;\s*--text-label--line-height:\s*1\.5;/);
  });

  it('allows one shadow only: the torn sealed slip', () => {
    expect(css).toContain('--shadow-*: initial;');
    expect(css).toContain('--inset-shadow-*: initial;');
    expect(css).toContain('--drop-shadow-*: initial;');
    expect([...css.matchAll(/^\s*--(?:inset-|drop-)?shadow-([a-z-]+):/gm)].map((m) => m[1])).toEqual(['torn']);
    expect(css).toContain('--drop-shadow-torn: 0 6px 10px rgb(15 61 58 / .2);');
  });

  it('has no gradients and no external URLs', () => {
    expect(css).not.toMatch(/gradient\(/);
    expect(css).not.toMatch(/https?:|googleapis|gstatic/);
  });

  it('removes animation and transitions under prefers-reduced-motion', () => {
    const block = /@media \(prefers-reduced-motion: reduce\)\s*{([^}]*{[^}]*})/.exec(css)?.[1] ?? '';
    expect(block).toContain('animation: none !important;');
    expect(block).toContain('transition-duration: .01ms !important;');
  });
});
