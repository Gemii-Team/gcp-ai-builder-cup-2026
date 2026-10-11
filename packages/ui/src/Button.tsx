import { useId, type ComponentPropsWithRef } from 'react';
import { cx } from './cx';

type ButtonProps = Omit<ComponentPropsWithRef<'button'>, 'type'> & {
  /** Primary is Ink Teal and there is one per region; secondary is Slip Paper with a Control Line border. */
  variant?: 'primary' | 'secondary';
  /** 36px, or 32px for `sm`; both are 44px under 1024px. */
  size?: 'md' | 'sm';
  /** Why the button is disabled. Only while it is disabled, it is exposed through `aria-describedby`. */
  disabledReason?: string;
  type?: 'button' | 'submit' | 'reset';
};

const VARIANT = {
  primary:
    'bg-primary border-primary text-on-primary hover:enabled:bg-primary-hover hover:enabled:border-primary-hover active:enabled:bg-primary-press',
  secondary:
    'bg-slip-paper border-control-line text-ledger-ink hover:enabled:bg-counter-hover hover:enabled:border-control-line-hover active:enabled:bg-citation-wash',
} as const;

const SIZE = {
  md: 'min-h-9 px-[14px] text-button',
  sm: 'min-h-8 px-2.5 text-[14px] leading-[1.5] font-medium',
} as const;

export function Button({
  variant = 'secondary',
  size = 'md',
  disabledReason,
  type = 'button',
  className,
  disabled,
  children,
  'aria-describedby': describedBy,
  ...rest
}: ButtonProps) {
  const reasonId = useId();
  const reason = disabled && disabledReason ? reasonId : undefined;
  return (
    <>
      <button
        type={type}
        disabled={disabled}
        aria-describedby={cx(describedBy, reason) || undefined}
        className={cx(
          'inline-flex items-center justify-center gap-2 rounded-md border whitespace-nowrap cursor-pointer transition-colors duration-150 max-lg:min-h-11',
          'disabled:bg-disabled-fill disabled:border-disabled-fill disabled:text-disabled-ink disabled:cursor-not-allowed',
          VARIANT[variant],
          SIZE[size],
          className,
        )}
        {...rest}
      >
        {children}
      </button>
      {reason !== undefined && (
        <span id={reason} className="sr-only">
          {disabledReason}
        </span>
      )}
    </>
  );
}
