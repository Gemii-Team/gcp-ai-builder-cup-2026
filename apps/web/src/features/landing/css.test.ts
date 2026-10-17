import { describe, expect, it } from 'vitest';
import channelCss from '../channel/channel.css?raw';
import shellCss from '../shell/shell.css?raw';
import signinCss from '../auth/signin.css?raw';
import landingCss from './landing.css?raw';

/** The class names a stylesheet's selectors use, comments left out. */
function classesOf(css: string): string[] {
  return [...css.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/\.(-?[_a-zA-Z][\w-]*)/g)].map((match) => match[1] ?? '');
}

describe('the landing stylesheet next to the app stylesheets', () => {
  // Vite ships every stylesheet to every route, so a short, app-wide name in the landing page would restyle the shell.
  it('names every class with the lp- prefix, so none can meet a class of the shell, the channel or sign-in', () => {
    const landing = classesOf(landingCss);
    expect(landing.length).toBeGreaterThan(0);
    expect(landing.filter((name) => !name.startsWith('lp-'))).toEqual([]);
    const others = new Set([...classesOf(shellCss), ...classesOf(channelCss), ...classesOf(signinCss)]);
    expect(landing.filter((name) => others.has(name))).toEqual([]);
  });

  // The design system has flat colour only, and every colour is a token.
  it('uses no gradient and no literal colour', () => {
    const rules = landingCss.replace(/\/\*[\s\S]*?\*\//g, '');
    expect(rules).not.toMatch(/gradient\(/);
    expect(rules).not.toMatch(/#[0-9a-fA-F]{3,8}\b|rgba?\(/);
  });
});
