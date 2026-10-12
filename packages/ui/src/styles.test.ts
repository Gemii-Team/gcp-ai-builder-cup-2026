import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const root = fileURLToPath(new URL('.', import.meta.url));
const files = readdirSync(root, { recursive: true, encoding: 'utf8' }).map((path) => path.replaceAll('\\', '/'));
const read = (path: string) => readFileSync(join(root, path), 'utf8');

// theme.css and fonts.css are the token sheets; every other stylesheet is a component's.
const componentCss = files.filter((path) => path.endsWith('.css') && path !== 'theme.css' && path !== 'fonts.css');
const componentSources = files.filter((path) => /\.tsx?$/.test(path) && !/\.test\.tsx?$/.test(path));
const HEX_COLOUR = /#[0-9a-fA-F]{3,8}\b/;

describe('component stylesheets', () => {
  it('includes the slip stylesheet', () => {
    expect(componentCss).toContain('slip/slip.css');
  });

  // The torn, sealed slip's wrapper is the one place the shadow token is used: draft/draft.css, whose single use
  // `OneShadowRule` (draft.test.tsx) pins. Nothing else casts a shadow, and nothing at all uses a box or text shadow.
  it.each(componentCss)('%s uses no box or text shadow', (path) => {
    expect(read(path)).not.toMatch(/box-shadow|text-shadow/);
  });

  it.each(componentCss.filter((path) => path !== 'draft/draft.css'))('%s casts no shadow: only the torn, sealed slip may', (path) => {
    expect(read(path)).not.toMatch(/drop-shadow/);
  });

  // The one URL a sheet may hold is the SVG namespace of an inline `data:` mask, which names a format and fetches nothing.
  it.each(componentCss)('%s has no gradient, no external URL and no literal hex colour', (path) => {
    const css = read(path);
    expect(css).not.toMatch(/gradient\(/);
    expect(css).not.toMatch(/https?:(?!\/\/www\.w3\.org\/2000\/svg)|googleapis|gstatic/);
    expect(css).not.toMatch(HEX_COLOUR);
  });
});

describe('component sources', () => {
  it.each(componentSources)('%s draws with token classes, never a literal hex colour', (path) => {
    expect(read(path)).not.toMatch(HEX_COLOUR);
  });
});

describe('slip.css', () => {
  const css = read('slip/slip.css');

  it('cuts the perforation as a 1.5px dashed Perforation Sage rule with two 7 by 13 notches in the ground colour', () => {
    expect(css).toMatch(/\.kiwi-perf\s*{[^}]*border-top:\s*1\.5px dashed var\(--color-perforation-sage\)/);
    expect(css).toMatch(/\.kiwi-perf::before,\s*\.kiwi-perf::after\s*{[^}]*width:\s*var\(--radius-notch\)/);
    expect(css).toMatch(/\.kiwi-perf::before,\s*\.kiwi-perf::after\s*{[^}]*height:\s*13px/);
    expect(css).toMatch(/\.kiwi-perf::before,\s*\.kiwi-perf::after\s*{[^}]*background:\s*var\(--color-counter-mint\)/);
    expect(css).toMatch(/\.kiwi-perf::before,\s*\.kiwi-perf::after\s*{[^}]*border:\s*1px solid var\(--color-slip-edge\)/);
  });

  it('lays tracks out as the 104px | 1fr | auto grid and reflows them on phones', () => {
    expect(css).toMatch(/\.kiwi-track\s*{[^}]*grid-template-columns:\s*104px minmax\(0,\s*1fr\) auto/);
    expect(css).toMatch(/@media \(max-width:\s*1023px\)\s*{[\s\S]*grid-template-areas:\s*"label offset" "text text"/);
  });

  it('underlines offset links in Perforation Sage and gives them a larger hit area, larger still on touch', () => {
    expect(css).toMatch(/\.kiwi-ts\s*{[^}]*text-decoration-color:\s*var\(--color-perforation-sage\)/);
    expect(css).toMatch(/\.kiwi-ts\s*{[^}]*text-underline-offset:\s*3px/);
    expect(css).toMatch(/\.kiwi-ts::after\s*{[^}]*inset:\s*-6px/);
    expect(css).toMatch(/@media \(max-width:\s*1023px\)\s*{[\s\S]*\.kiwi-ts::after\s*{[^}]*inset:\s*-14px -10px/);
  });

  it('drops the bar animation under prefers-reduced-motion as well as in script', () => {
    expect(css).toMatch(/@media \(prefers-reduced-motion:\s*reduce\)\s*{[\s\S]*\.kiwi-pulse[\s\S]*animation:\s*none/);
  });
});
