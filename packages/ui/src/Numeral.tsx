import type { ReactNode } from 'react';
import { cx } from './cx';

/** The Two Voices Rule: anything counted, timed or referenced is Noto Sans Mono, tabular, and never wraps. */
export function Numeral({ children, className }: { children: ReactNode; className?: string }) {
  return <span className={cx('font-mono text-numeral whitespace-nowrap', className)}>{children}</span>;
}
