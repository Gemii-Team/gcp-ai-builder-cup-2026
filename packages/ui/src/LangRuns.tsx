import type { ReactNode } from 'react';

// The Thai Tag Rule: every run of Thai carries lang="th", and a Latin marker inside Thai prose lang="en".
// English is the document's default language, so English text is never wrapped.

/**
 * A Thai run: Thai characters plus the Latin words, digits and punctuation between them (U+0E00-U+0E7F). It is global,
 * so it is only ever used through `matchAll`, never `test`, which would carry `lastIndex` between calls.
 */
const THAI_RUN = /[\u0E00-\u0E7F](?:[\u0E00-\u0E7F\w\s.,!?:;()'"%/–-]*[\u0E00-\u0E7F])?/g;

/** `text` with every Thai run wrapped in `<span lang="th">`; the rest stays plain text. */
export function langRuns(text: string): ReactNode[] {
  const nodes: ReactNode[] = [];
  let last = 0;
  for (const run of text.matchAll(THAI_RUN)) {
    if (run.index > last) {
      nodes.push(text.slice(last, run.index));
    }
    nodes.push(
      <span key={run.index} lang="th">
        {run[0]}
      </span>,
    );
    last = run.index + run[0].length;
  }
  if (last < text.length) {
    nodes.push(text.slice(last));
  }
  return nodes;
}

export function LangRuns({ text }: { text: string }) {
  return <>{langRuns(text)}</>;
}

/** A Latin marker inside Thai prose, such as the `[1]` of a citation. */
export function EnMarker({ children }: { children: ReactNode }) {
  return <span lang="en">{children}</span>;
}

const EN_TAG = /<en>(.*?)<\/en>/gs;

/**
 * A translated string whose Latin words are marked `<en>Google</en>`: each becomes a `lang="en"` span. Everything
 * else renders as plain text (the surrounding document already carries the Thai language), so no markup is ever
 * interpreted.
 */
export function Rich({ text }: { text: string }) {
  const nodes: ReactNode[] = [];
  let last = 0;
  for (const tag of text.matchAll(EN_TAG)) {
    if (tag.index > last) {
      nodes.push(text.slice(last, tag.index));
    }
    nodes.push(<EnMarker key={tag.index}>{tag[1]}</EnMarker>);
    last = tag.index + tag[0].length;
  }
  if (last < text.length) {
    nodes.push(text.slice(last));
  }
  return <>{nodes}</>;
}
