import type { ReactNode } from 'react';
import { cx } from './cx';

/**
 * The Two Voices Rule: anything counted, timed or referenced is Noto Sans Mono, tabular, and never wraps. `lang` is for
 * the Latin ones (a REF code) inside Thai prose.
 */
export function Numeral({ children, className, lang }: { children: ReactNode; className?: string; lang?: string }) {
  return (
    <span lang={lang} className={cx('font-mono text-numeral whitespace-nowrap', className)}>
      {children}
    </span>
  );
}
