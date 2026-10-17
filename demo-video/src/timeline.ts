import type { SceneRec } from './manifest';
import { CLOSE_RANGE, SCENES, TARGET_SECONDS, type SceneSpec } from './script';

/** One scene placed on the composition's timeline. */
export type Segment = SceneSpec & {
  /** The first frame of the segment and how many it lasts. */
  from: number;
  duration: number;
  /** The slice of the recording the segment plays, in seconds from the first captured frame; null for a card or without a recording. */
  rec: { t0: number; t1: number } | null;
};

/**
 * Lays the scenes out one after another. A recorded scene lasts the longer of its script seconds and its recording (plus a
 * beat); without a recording every screen scene stands in with a placeholder of its script length, so the cards can be
 * previewed before anything is captured. A screen scene the recording does not have is left out.
 */
export function buildTimeline(scenes: readonly SceneRec[] | null, fps: number): { segments: Segment[]; total: number } {
  const segments: Segment[] = [];
  let from = 0;
  for (const spec of SCENES) {
    let rec: Segment['rec'] = null;
    let seconds = spec.seconds;
    if (spec.kind === 'screen' && scenes !== null) {
      const scene = scenes.find((candidate) => candidate.id === spec.id);
      if (scene === undefined) {
        continue;
      }
      rec = { t0: scene.t0, t1: scene.t1 };
      seconds = Math.max(spec.seconds, scene.t1 - scene.t0 + 0.4);
    }
    const duration = Math.max(1, Math.round(seconds * fps));
    segments.push({ ...spec, from, duration, rec });
    from += duration;
  }
  // Land on TARGET_SECONDS by giving the close card whatever the other scenes leave (within CLOSE_RANGE).
  const close = segments.find((segment) => segment.id === 'close');
  if (close !== undefined && segments[segments.length - 1] === close) {
    const others = from - close.duration;
    const wanted = Math.round(TARGET_SECONDS * fps) - others;
    close.duration = Math.min(Math.round(CLOSE_RANGE.max * fps), Math.max(Math.round(CLOSE_RANGE.min * fps), wanted));
    from = others + close.duration;
  }
  return { segments, total: Math.max(1, from) };
}

/** The second of the recording a segment shows at its local frame: real time while there is recording, a held frame to fill the rest. */
export function recordingTime(segment: Segment, localFrame: number, fps: number): number {
  if (segment.rec === null) {
    return 0;
  }
  const t = localFrame / fps;
  const slot = segment.duration / fps;
  const length = segment.rec.t1 - segment.rec.t0;
  if (segment.hold === 'stretch') {
    // Slower than real time, so the moments of the recording line up with the narration across the whole scene.
    return segment.rec.t0 + Math.min(length, (t * length) / slot);
  }
  if (segment.hold === 'start') {
    return segment.rec.t0 + Math.max(0, Math.min(length, t - Math.max(0, slot - length)));
  }
  return segment.rec.t0 + Math.min(length, t);
}

/**
 * The local frame of a segment at which the recording reaches second `t`: the inverse of `recordingTime`. Null when the
 * segment does not play that second.
 */
export function frameOfRecording(segment: Segment, t: number, fps: number): number | null {
  if (segment.rec === null || t < segment.rec.t0 || t > segment.rec.t1) {
    return null;
  }
  const slot = segment.duration / fps;
  const length = segment.rec.t1 - segment.rec.t0;
  const into = t - segment.rec.t0;
  const seconds = segment.hold === 'stretch' ? (into * slot) / Math.max(length, 1e-6) : segment.hold === 'start' ? into + Math.max(0, slot - length) : into;
  const frame = Math.round(seconds * fps);
  return frame < segment.duration ? frame : null;
}
