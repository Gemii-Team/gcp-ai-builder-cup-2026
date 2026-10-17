import { useEffect } from 'react';
import { continueRender, delayRender, staticFile } from 'remotion';

const THAI = 'U+0E01-0E5B, U+200C-200D, U+25CC';
const LATIN =
  'U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD';

const face = (family: string, file: string, weight: number, range: string) =>
  `@font-face{font-family:"${family}";font-style:normal;font-weight:${weight};font-display:block;src:url("${staticFile(`fonts/${file}.woff2`)}") format("woff2");unicode-range:${range};}`;

const css = [400, 500, 600, 700]
  .flatMap((weight) => [face('Anuphan', `anuphan-thai-${weight}-normal`, weight, THAI), face('Anuphan', `anuphan-latin-${weight}-normal`, weight, LATIN)])
  .concat([500, 600].map((weight) => face('Noto Sans Mono', `noto-sans-mono-latin-${weight}-normal`, weight, LATIN)))
  .join('\n');

/** The app's own faces (self-hosted, Thai and Latin), declared once; the render waits until they are loaded. */
export function Fonts() {
  useEffect(() => {
    const handle = delayRender('Loading Anuphan and Noto Sans Mono');
    const loads = [400, 500, 600, 700].flatMap((weight) => [document.fonts.load(`${weight} 20px Anuphan`, 'Aa'), document.fonts.load(`${weight} 20px Anuphan`, 'กข')]);
    loads.push(document.fonts.load('500 20px "Noto Sans Mono"', '01'), document.fonts.load('600 20px "Noto Sans Mono"', '01'));
    Promise.all(loads).then(
      () => continueRender(handle),
      () => continueRender(handle),
    );
  }, []);
  return <style>{css}</style>;
}
