import type { ComponentPropsWithoutRef } from 'react';
import { cx } from '../cx';

/** The part of a slip below its perforation: REF, time and sources. */
export function Stub({ className, ...rest }: ComponentPropsWithoutRef<'div'>) {
  return <div className={cx('kiwi-stub', className)} {...rest} />;
}

/** One line of a stub: something at the left, something at the right. */
export function StubRow({ className, ...rest }: ComponentPropsWithoutRef<'div'>) {
  return <div className={cx('kiwi-row', className)} {...rest} />;
}
