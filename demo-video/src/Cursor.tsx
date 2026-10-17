import type { CursorRec } from './manifest';
import { color } from './theme';

type CursorLayerProps = {
  /** Where the pointer is, in output pixels; null hides it. */
  at: { x: number; y: number } | null;
  /** Recent presses, in output pixels, with their age in seconds. */
  presses: readonly (CursorRec & { age: number })[];
};

const RIPPLE = 0.45;

/** A pointer drawn over the captured screen (a screencast has none), with a ripple where it pressed. */
export function CursorLayer({ at, presses }: CursorLayerProps) {
  return (
    <svg width={1920} height={1080} viewBox="0 0 1920 1080" style={{ position: 'absolute', inset: 0, pointerEvents: 'none' }}>
      {presses.map((press) => {
        const p = Math.min(1, press.age / RIPPLE);
        return <circle key={press.t} cx={press.x} cy={press.y} r={8 + 34 * p} fill="rgba(143,227,190,.25)" stroke={color.sealGreen} strokeWidth={3} opacity={1 - p} />;
      })}
      {at !== null && (
        <g transform={`translate(${at.x - 6} ${at.y - 4})`}>
          <path
            d="M5 3 L5 24.5 L10.6 19 L14.2 27.6 L17.8 26 L14.2 17.6 L21.4 17.6 Z"
            transform="scale(1.35)"
            fill="#fff"
            stroke="#15211F"
            strokeWidth="1.4"
            strokeLinejoin="round"
            style={{ filter: 'drop-shadow(0 2px 3px rgba(0,0,0,.35))' }}
          />
        </g>
      )}
    </svg>
  );
}
