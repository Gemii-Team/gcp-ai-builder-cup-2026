import { useCallback, useEffect, useRef, useState } from 'react';

/** A live region only speaks a change, so the text is cleared first and set again a moment later. */
const SAY_DELAY_MS = 40;

/** The text of a `role="status"` live region and the function that says something in it, even the same thing twice. */
export function useAnnouncer(): [text: string, say: (text: string) => void] {
  const [text, setText] = useState('');
  const timer = useRef<number>(undefined);

  useEffect(
    () => () => {
      window.clearTimeout(timer.current);
    },
    [],
  );

  const say = useCallback((next: string) => {
    window.clearTimeout(timer.current);
    setText('');
    timer.current = window.setTimeout(() => {
      setText(next);
    }, SAY_DELAY_MS);
  }, []);

  return [text, say];
}
