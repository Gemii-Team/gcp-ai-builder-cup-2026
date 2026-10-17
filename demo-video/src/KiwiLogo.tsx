import { color } from './theme';

/** The K.I.W.I. kiwi bird (apps/web/public/brand/kiwi-mascot.svg) on a paper tile: the logo of the cards. */
export function KiwiLogo({ size }: { size: number }) {
  return (
    <svg viewBox="0 0 96 96" width={size} height={size} aria-hidden="true">
      <rect x="2" y="2" width="92" height="92" rx="22" fill={color.slipPaper} />
      <g transform="translate(6 3) scale(0.86)">
        <ellipse cx="44" cy="56" rx="28" ry="22" fill={color.primary} />
        <circle cx="64" cy="40" r="12" fill={color.primary} />
        <circle cx="68" cy="37" r="3.5" fill={color.slipPaper} />
        <circle cx="69" cy="37" r="1.75" fill={color.ledgerInk} />
        <path d="M74 42 Q84 48 90 62" fill="none" stroke={color.billTan} strokeWidth="3.5" strokeLinecap="round" />
        <path d="M36 77 V86 M32 86 H40 M52 77 V86 M48 86 H56" fill="none" stroke={color.billTan} strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" />
      </g>
    </svg>
  );
}
