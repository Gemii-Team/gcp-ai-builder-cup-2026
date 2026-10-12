import { afterEach, describe, expect, it } from 'vitest';
import channelCss from '../channel/channel.css?raw';
import shellCss from '../shell/shell.css?raw';
import signinCss from './signin.css?raw';

/** The class names a stylesheet's selectors use, comments left out. */
function classesOf(css: string): string[] {
  return [...css.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/\.(-?[_a-zA-Z][\w-]*)/g)].map((match) => match[1] ?? '');
}

afterEach(() => {
  document.head.replaceChildren();
  document.body.replaceChildren();
});

describe('the sign-in stylesheet next to the shell stylesheets', () => {
  // shell.css and channel.css name their classes short and app-wide (.main, .ch, .me, .av, .sheet, .scrim, .notice ...)
  // and Vite ships them to every route, the sign-in page included.
  it('names every class with the si- prefix, so none can meet a class of the shell or the channel', () => {
    const signin = classesOf(signinCss);
    expect(signin.length).toBeGreaterThan(0);
    expect(signin.filter((name) => !name.startsWith('si-'))).toEqual([]);
    const others = new Set([...classesOf(shellCss), ...classesOf(channelCss)]);
    expect(signin.filter((name) => others.has(name))).toEqual([]);
  });

  // jsdom resolves the cascade of a stylesheet it is given, `:has()` included, but lays nothing out: this pins which
  // pages the lock reaches, not how the page then scrolls in a browser.
  it('locks the page scroll only while the signed-in shell is mounted, so the sign-in page scrolls on a short phone', () => {
    const style = document.createElement('style');
    style.textContent = shellCss;
    document.head.append(style);
    const root = document.createElement('div');
    root.id = 'root';
    document.body.append(root);

    expect(getComputedStyle(document.documentElement).overflow).not.toBe('hidden');
    expect(getComputedStyle(document.body).overflow).not.toBe('hidden');
    expect(getComputedStyle(root).height).not.toBe('100%');

    const shell = document.createElement('div');
    shell.className = 'shell';
    root.append(shell);

    expect(getComputedStyle(document.documentElement).overflow).toBe('hidden');
    expect(getComputedStyle(document.body).overflow).toBe('hidden');
    expect(getComputedStyle(root).height).toBe('100%');

    shell.remove();

    expect(getComputedStyle(document.body).overflow).not.toBe('hidden');
  });
});
