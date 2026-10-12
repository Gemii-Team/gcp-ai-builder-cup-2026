import { useId, type SVGProps } from 'react';
import { cx } from './cx';
import { useLabels } from './labels';

// Brand drawings ported from .impeccable/brand and the prototypes. In the app they are drawn with token
// classes (Ink Teal, Slip Paper, Seal Green ...) rather than literal colours, so a palette change moves them.

type DrawingProps = Omit<SVGProps<SVGSVGElement>, 'children' | 'width' | 'height' | 'viewBox'>;

type MarkProps = DrawingProps & { size: 40 | 48 | 64; label?: string };

/** The Mark Size Rule: the ringed mark is for 40px and up. Below that use `MarkSmall`. */
export function Mark({ size, label, ...rest }: MarkProps) {
  return (
    <svg
      viewBox="0 0 48 48"
      width={size}
      height={size}
      {...(label === undefined ? { 'aria-hidden': true } : { role: 'img', 'aria-label': label })}
      {...rest}
    >
      <rect className="fill-slip-paper" x="2" y="2" width="44" height="44" rx="10" />
      <circle
        className="stroke-primary"
        cx="24"
        cy="24"
        r="18"
        fill="none"
        strokeWidth="1.5"
        strokeDasharray="3 2.2"
        strokeLinecap="round"
      />
      <rect className="fill-primary" x="14" y="12" width="4" height="24" rx="0.5" />
      <path className="fill-primary" d="M18 24 L28 12 H34 L24 24 L34 36 H28 Z" />
    </svg>
  );
}

type MarkSmallProps = DrawingProps & { size: 16 | 20 | 24; label?: string };

/** The Mark Size Rule: the plain mark, without the ring, is for 24px and below. */
export function MarkSmall({ size, label, ...rest }: MarkSmallProps) {
  return (
    <svg
      viewBox="0 0 16 16"
      width={size}
      height={size}
      {...(label === undefined ? { 'aria-hidden': true } : { role: 'img', 'aria-label': label })}
      {...rest}
    >
      <rect className="fill-primary" width="16" height="16" rx="4" />
      <rect className="fill-slip-paper" x="4" y="3" width="2" height="10" />
      <path className="fill-slip-paper" d="M6 8 L10 3 H12.6 L8.6 8 L12.6 13 H10 Z" />
    </svg>
  );
}

/**
 * The CONFIRMED stamp: the only Seal Green drawing. 80px, rotated -8 degrees through `transform` (not the `rotate`
 * property), so the stamp-down animation can take over the same property; a layout may shrink it by class.
 */
export function Seal({ className, ...rest }: DrawingProps) {
  const arc = useId();
  const labels = useLabels();
  return (
    <svg
      viewBox="0 0 80 80"
      width="80"
      height="80"
      role="img"
      aria-label={labels.text('confirmed')}
      className={cx('[transform:rotate(-8deg)]', className)}
      {...rest}
    >
      <defs>
        <path id={arc} d="M12 40A28 28 0 0 0 68 40" />
      </defs>
      <circle className="fill-seal-wash stroke-seal-green" cx="40" cy="40" r="36" strokeWidth="2" />
      <circle className="stroke-seal-green" cx="40" cy="40" r="32" fill="none" strokeWidth="1" strokeDasharray="2 2" />
      <path
        className="stroke-seal-green"
        d="M28 36l8 8 16-16"
        fill="none"
        strokeWidth="3"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <text
        className="fill-seal-ink font-sans"
        fontSize="11"
        fontWeight="700"
        letterSpacing={labels.lang === 'en' ? '.5' : undefined}
        textAnchor="middle"
      >
        <textPath href={`#${arc}`} startOffset="50%">
          {labels.text('confirmedStamp')}
        </textPath>
      </text>
    </svg>
  );
}

type MascotProps = DrawingProps & {
  variant: 'plain' | 'sealed';
  /** Width in pixels: 176 in the drafts empty state, 132 in the Board's drafts band, 112 beside the sign-in slip. */
  size: number;
};

/** The kiwi stands only where a screen has nothing else to say: empty states and onboarding. */
export function Mascot({ variant, size, ...rest }: MascotProps) {
  const sealed = variant === 'sealed';
  const height = sealed ? (size * 104) / 176 : size;
  const kiwi = (
    <>
      <ellipse className="fill-primary" cx="44" cy="56" rx="28" ry="22" />
      <circle className="fill-primary" cx="64" cy="40" r="12" />
      <circle className="fill-slip-paper" cx="68" cy="37" r="3.5" />
      <circle className="fill-ledger-ink" cx="69" cy="37" r="1.75" />
      <path
        className="stroke-bill-tan"
        d={sealed ? 'M74 42 Q80 52 78 66' : 'M74 42 Q84 48 90 62'}
        fill="none"
        strokeWidth="3.5"
        strokeLinecap="round"
      />
      <path
        className="stroke-bill-tan"
        d="M36 77 V86 M32 86 H40 M52 77 V86 M48 86 H56"
        fill="none"
        strokeWidth="3"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </>
  );
  return (
    <svg
      viewBox={sealed ? '0 0 176 104' : '0 0 96 96'}
      width={size}
      height={height}
      aria-hidden="true"
      focusable="false"
      {...rest}
    >
      {sealed ? (
        <>
          <path className="fill-slip-paper stroke-perforation-sage" d="M8 12 H88 V56 H8 Z" strokeWidth="1" />
          <path className="stroke-perforation-sage" d="M8 56 H88" fill="none" strokeWidth="1" strokeDasharray="4 3" />
          <path
            className="fill-slip-paper stroke-perforation-sage"
            d="M8 56 L14 62 L20 56 L26 62 L32 56 L38 62 L44 56 L50 62 L56 56 L62 62 L68 56 L74 62 L80 56 L88 62 V56"
            strokeWidth="1"
            strokeLinejoin="round"
          />
          <rect className="fill-perforation-sage" x="16" y="20" width="40" height="4" rx="2" />
          <rect className="fill-tally-mint" x="16" y="32" width="28" height="4" rx="2" />
          <rect className="fill-tally-mint" x="16" y="42" width="36" height="4" rx="2" />
          <circle className="stroke-seal-green" cx="72" cy="38" r="12" fill="none" strokeWidth="2" />
          <circle className="stroke-seal-green" cx="72" cy="38" r="9" fill="none" strokeWidth="1" strokeDasharray="2 1.6" />
          <path
            className="stroke-seal-green"
            d="M67 38.5 L70.5 42 L77 34.5"
            fill="none"
            strokeWidth="2.2"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
          <g transform="translate(88 8)">{kiwi}</g>
        </>
      ) : (
        kiwi
      )}
    </svg>
  );
}
