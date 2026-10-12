import { existsSync, readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const require = createRequire(import.meta.url);
const fontsCssPath = new URL('./fonts.css', import.meta.url);
const fontsCss = readFileSync(fontsCssPath, 'utf8');
const indexHtml = readFileSync(new URL('../../../apps/web/index.html', import.meta.url), 'utf8');

interface Face {
  family: string;
  weight: number;
  display: string | undefined;
  unicodeRange: string | undefined;
  woff2: string;
}

/** The `@font-face` rules of every stylesheet fonts.css imports, with their woff2 file resolved to disk. */
function importedFaces(): Face[] {
  const faces: Face[] = [];
  for (const match of fontsCss.matchAll(/^@import\s+["']([^"']+)["'];/gm)) {
    const sheet = require.resolve(match[1]);
    const body = readFileSync(sheet, 'utf8');
    for (const rule of body.matchAll(/@font-face\s*{([^}]*)}/g)) {
      const declarations = rule[1];
      const descriptor = (name: string) => new RegExp(`${name}:\\s*([^;]+);`).exec(declarations)?.[1]?.trim();
      const woff2 = /url\(([^)]+\.woff2)\)\s*format\('woff2'\)/.exec(declarations)?.[1];
      faces.push({
        family: descriptor('font-family')!.replace(/['"]/g, ''),
        weight: Number(descriptor('font-weight')),
        display: descriptor('font-display'),
        unicodeRange: descriptor('unicode-range'),
        woff2: resolve(dirname(sheet), woff2 ?? ''),
      });
    }
  }
  return faces;
}

describe('fonts.css is self-hosted', () => {
  it('names no CDN and no external URL in fonts.css or index.html', () => {
    for (const source of [fontsCss, indexHtml]) {
      expect(source).not.toMatch(/googleapis/);
      expect(source).not.toMatch(/gstatic/);
      expect(source).not.toMatch(/http/);
    }
  });

  it('serves every font file from node_modules: each woff2 exists on disk', () => {
    const faces = importedFaces();
    expect(faces.length).toBeGreaterThan(0);
    for (const face of faces) {
      expect(existsSync(face.woff2), face.woff2).toBe(true);
    }
  });

  it('swaps in the font after first paint (font-display: swap) for every face', () => {
    for (const face of importedFaces()) {
      expect(face.display, `${face.family} ${face.weight} ${face.woff2}`).toBe('swap');
    }
  });
});

describe('fonts.css families', () => {
  it.each([400, 500, 600, 700])('Anuphan %i covers the Thai block and Latin text', (weight) => {
    const faces = importedFaces().filter((face) => face.family === 'Anuphan' && face.weight === weight);
    const thai = faces.filter((face) => face.woff2.includes('-thai-'));
    const latin = faces.filter((face) => face.woff2.includes('-latin-') && !face.woff2.includes('-latin-ext-'));
    expect(thai).toHaveLength(1);
    expect(latin).toHaveLength(1);
    expect(thai[0].unicodeRange ?? 'no unicode-range').toContain('U+0E01-0E5B');
    expect(latin[0].unicodeRange ?? 'no unicode-range').toContain('U+0000-00FF');
  });

  it.each([400, 500, 600])('Noto Sans Mono %i covers Latin text, numerals included', (weight) => {
    const faces = importedFaces().filter((face) => face.family === 'Noto Sans Mono' && face.weight === weight);
    expect(faces).toHaveLength(1);
    expect(faces[0].woff2).toContain('-latin-');
  });

  it('loads Anuphan at exactly 400, 500, 600 and 700 and Noto Sans Mono at exactly 400, 500 and 600', () => {
    const weights = (family: string) =>
      [...new Set(importedFaces().filter((face) => face.family === family).map((face) => face.weight))].sort(
        (a, b) => a - b,
      );
    expect(weights('Anuphan')).toEqual([400, 500, 600, 700]);
    expect(weights('Noto Sans Mono')).toEqual([400, 500, 600]);
  });

  // Fontsource's per-subset files (thai-400.css, latin-400.css) carry no unicode-range: two of them for one
  // family and weight are two faces that both claim every code point, and the later one wins for all text.
  it('never gives a family and weight two faces unless each has its own unicode-range', () => {
    const faces = importedFaces();
    for (const face of faces) {
      const siblings = faces.filter((other) => other.family === face.family && other.weight === face.weight);
      if (siblings.length > 1) {
        expect(face.unicodeRange, `${face.family} ${face.weight} ${face.woff2}`).toBeDefined();
      }
    }
  });
});
