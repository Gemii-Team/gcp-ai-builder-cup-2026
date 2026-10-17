import { interpolate, useCurrentFrame, useVideoConfig } from 'remotion';
import type { Segment } from './timeline';
import { color, font } from './theme';

/** Each caption gets a share of its scene in proportion to its length, with a short breath between two. */
function windows(segment: Segment, fps: number): { text: string; from: number; to: number }[] {
  const gap = Math.round(fps * 0.25);
  const usable = segment.duration - gap * Math.max(0, segment.lines.length - 1);
  const weights = segment.lines.map((line) => line.length);
  const total = weights.reduce((sum, w) => sum + w, 0);
  let at = segment.from;
  return segment.lines.map((text, i) => {
    const length = Math.round((usable * weights[i]!) / total);
    const from = at;
    at += length + gap;
    return { text, from, to: from + length };
  });
}

/** The narration of the scene, one line at a time, in a band under the screen. Turn it off for the cut that carries the avatar's voice. */
export function Captions({ segments, narrow = false }: { segments: readonly Segment[]; narrow?: boolean }) {
  const frame = useCurrentFrame();
  const { fps } = useVideoConfig();
  const segment = segments.find((candidate) => frame >= candidate.from && frame < candidate.from + candidate.duration);
  if (segment === undefined || segment.lines.length === 0) {
    return null;
  }
  const current = windows(segment, fps).find((w) => frame >= w.from && frame < w.to);
  if (current === undefined) {
    return null;
  }
  const opacity = interpolate(frame, [current.from, current.from + 6, current.to - 6, current.to], [0, 1, 1, 0], { extrapolateLeft: 'clamp', extrapolateRight: 'clamp' });
  const rise = interpolate(frame, [current.from, current.from + 8], [8, 0], { extrapolateLeft: 'clamp', extrapolateRight: 'clamp' });
  return (
    <div style={{ position: 'absolute', left: 0, right: narrow ? 300 : 0, bottom: 28, display: 'flex', justifyContent: 'center', pointerEvents: 'none' }}>
      <div
        style={{
          maxWidth: narrow ? 1180 : 1340,
          padding: '12px 26px',
          borderRadius: 14,
          background: 'rgba(10,45,43,.82)',
          color: color.slipPaper,
          fontFamily: font.sans,
          fontSize: 31,
          fontWeight: 500,
          lineHeight: 1.4,
          textAlign: 'center',
          opacity,
          transform: `translateY(${rise}px)`,
          boxShadow: '0 8px 30px rgba(0,0,0,.25)',
        }}
      >
        {current.text}
      </div>
    </div>
  );
}
