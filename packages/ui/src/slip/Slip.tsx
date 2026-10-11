import type { ComponentPropsWithoutRef } from 'react';
import { cx } from '../cx';

type SlipProps = ComponentPropsWithoutRef<'div'> & {
  /** `failed` is the Returned family: Returned Wash, Returned Edge and notches to match. */
  tone?: 'paper' | 'failed';
};

/** A piece of warm paper: Slip Paper, a 1px Slip Edge border, 12px corners, no shadow. */
export function Slip({ tone = 'paper', className, ...rest }: SlipProps) {
  return <div className={cx('kiwi-slip', className)} data-tone={tone === 'failed' ? 'failed' : undefined} {...rest} />;
}

/** The part of a slip above its perforation. */
export function SlipBody({ className, ...rest }: ComponentPropsWithoutRef<'div'>) {
  return <div className={cx('kiwi-slip-body', className)} {...rest} />;
}
