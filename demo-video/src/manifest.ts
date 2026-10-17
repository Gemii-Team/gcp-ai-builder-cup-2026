/** What `scripts/record.ts` writes to public/scenes.json: the captured frames, the cursor's path and the scene marks, all timed in seconds from the first frame. */

export type FrameRec = { t: number; f: string };
export type CursorRec = { t: number; x: number; y: number; k: 'm' | 'd' | 'u' };
export type SceneRec = { id: string; t0: number; t1: number };
export type EventRec = { t: number; k: 'chime' };

export type Manifest = {
  recordedAt: string;
  viewport: { width: number; height: number };
  output: { width: number; height: number };
  frames: FrameRec[];
  cursor: CursorRec[];
  scenes: SceneRec[];
  /** Moments the soundtrack marks; absent in a recording made before they existed. */
  events?: EventRec[];
};

/** The index of the last frame captured at or before `t`; -1 before the first one. */
export function frameAt(frames: readonly FrameRec[], t: number): number {
  let lo = 0;
  let hi = frames.length - 1;
  let found = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (frames[mid]!.t <= t) {
      found = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return found;
}

/** Where the cursor is at `t`: between the two samples around it, or on the last one before it. Null before the first sample. */
export function cursorAt(cursor: readonly CursorRec[], t: number): { x: number; y: number } | null {
  let lo = 0;
  let hi = cursor.length - 1;
  let found = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (cursor[mid]!.t <= t) {
      found = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  if (found < 0) {
    return null;
  }
  const a = cursor[found]!;
  const b = cursor[found + 1];
  if (b === undefined || b.t <= a.t) {
    return { x: a.x, y: a.y };
  }
  // Only a move is interpolated towards; a press stays where it is.
  const p = Math.min(1, (t - a.t) / (b.t - a.t));
  return { x: a.x + (b.x - a.x) * p, y: a.y + (b.y - a.y) * p };
}

/** The presses (mouse-down samples) in the `window` seconds up to `t`, for the click ripple. */
export function pressesNear(cursor: readonly CursorRec[], t: number, window: number): CursorRec[] {
  const out: CursorRec[] = [];
  for (let i = cursor.length - 1; i >= 0; i--) {
    const c = cursor[i]!;
    if (c.t > t) {
      continue;
    }
    if (c.t < t - window) {
      break;
    }
    if (c.k === 'd') {
      out.push(c);
    }
  }
  return out;
}
