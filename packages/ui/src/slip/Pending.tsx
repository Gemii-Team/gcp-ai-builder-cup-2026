import { useSyncExternalStore } from 'react';
import { cx } from '../cx';
import { Icon } from '../icons';
import { LangRuns } from '../LangRuns';

const REDUCED_MOTION = '(prefers-reduced-motion: reduce)';

function subscribe(onChange: () => void) {
  if (typeof window.matchMedia !== 'function') {
    return () => undefined;
  }
  const query = window.matchMedia(REDUCED_MOTION);
  query.addEventListener('change', onChange);
  return () => {
    query.removeEventListener('change', onChange);
  };
}

function snapshot() {
  return typeof window.matchMedia === 'function' && window.matchMedia(REDUCED_MOTION).matches;
}

/** Whether the user asked for less motion: components then drop their animation classes outright. */
export function usePrefersReducedMotion(): boolean {
  return useSyncExternalStore(subscribe, snapshot, () => false);
}

/** The 96 by 8 bar that pulses between full and half opacity over 2.4s, and holds still under reduced motion. */
export function PendingBar() {
  const reduced = usePrefersReducedMotion();
  return <span aria-hidden="true" className={cx('kiwi-pbar', !reduced && 'kiwi-pulse')} />;
}

/** The inline pending row: a clock icon, a plain sentence and the pulsing bar. It wraps when narrow. */
export function Pending({ label }: { label: string }) {
  return (
    <p role="status" className="kiwi-pending">
      <Icon name="clock" />
      <span>
        <LangRuns text={label} />
      </span>
      <PendingBar />
    </p>
  );
}
