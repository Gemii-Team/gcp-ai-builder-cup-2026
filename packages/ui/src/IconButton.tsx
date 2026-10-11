import type { ComponentPropsWithRef } from 'react';
import { cx } from './cx';

type IconButtonProps = Omit<ComponentPropsWithRef<'button'>, 'type' | 'aria-label'> & {
  /** The button's accessible name: the glyph inside carries none. */
  label: string;
  type?: 'button' | 'submit' | 'reset';
};

/** A 40px square (44px under 1024px) with an Ink Teal glyph, Citation Wash on hover and Mint Press when pressed. */
export function IconButton({ label, type = 'button', className, children, ...rest }: IconButtonProps) {
  return (
    <button
      type={type}
      aria-label={label}
      className={cx(
        'inline-grid size-10 place-items-center rounded-md border-0 bg-transparent text-primary cursor-pointer transition-colors duration-150 hover:bg-citation-wash active:bg-mint-press max-lg:size-11',
        className,
      )}
      {...rest}
    >
      {children}
    </button>
  );
}
