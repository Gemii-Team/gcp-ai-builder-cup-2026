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

/** A HeyGen clip for a scene: its file under public/, its length, and whether it has a picture (an avatar) or only sound. */
export type VoiceClip = { file: string; seconds: number; video: boolean };
export type VoiceClips = Record<string, VoiceClip>;

/** The shortest a scene may be, whatever is in it: a card still needs a moment to read. */
const MIN_CARD_SECONDS = 6;
/** A breath after a voice clip ends, before the next scene starts. */
const AFTER_VOICE = 0.5;

/**
 * Lays the scenes out one after another and lands on TARGET_SECONDS.
 *
 * Every scene has a floor it cannot go under (its recording plus a beat, its voice clip plus a breath) and a preferred
 * length (the larger of the floor and the script's seconds). The close card then takes what is left of TARGET_SECONDS,
 * within CLOSE_RANGE. When the preferred lengths leave too little room, the spare time above each floor is given back in
 * proportion, so the voice and the recording are never cut. Without a recording every screen scene stands in with a
 * placeholder; a screen scene the recording does not have is left out.
 */
export function buildTimeline(scenes: readonly SceneRec[] | null, fps: number, voice: VoiceClips = {}): { segments: Segment[]; total: number } {
  type Planned = { spec: SceneSpec; rec: Segment['rec']; floor: number; preferred: number };
  const planned: Planned[] = [];
  for (const spec of SCENES) {
    let rec: Segment['rec'] = null;
    let floor = spec.kind === 'screen' ? 1 : MIN_CARD_SECONDS;
    if (spec.kind === 'screen' && scenes !== null) {
      const scene = scenes.find((candidate) => candidate.id === spec.id);
      if (scene === undefined) {
        continue;
      }
      rec = { t0: scene.t0, t1: scene.t1 };
      floor = Math.max(floor, scene.t1 - scene.t0 + 0.4);
    }
    const clip = voice[spec.id];
    if (clip !== undefined) {
      floor = Math.max(floor, clip.seconds + AFTER_VOICE);
    }
    planned.push({ spec, rec, floor, preferred: Math.max(spec.seconds, floor) });
  }

  // The close card absorbs the difference; the scenes before it give back spare time if they leave it too little.
  const close = planned[planned.length - 1]?.spec.id === 'close' ? planned[planned.length - 1]! : undefined;
  const body = close === undefined ? planned : planned.slice(0, -1);
  const closeMin = close === undefined ? 0 : Math.max(CLOSE_RANGE.min, close.floor);
  const closeMax = close === undefined ? 0 : Math.max(CLOSE_RANGE.max, closeMin);
  let bodySeconds = body.reduce((sum, p) => sum + p.preferred, 0);
  const room = TARGET_SECONDS - closeMin;
  if (close !== undefined && bodySeconds > room) {
    const spare = body.reduce((sum, p) => sum + (p.preferred - p.floor), 0);
    const give = Math.min(spare, bodySeconds - room);
    for (const p of body) {
      p.preferred -= spare > 0 ? ((p.preferred - p.floor) / spare) * give : 0;
    }
    bodySeconds = body.reduce((sum, p) => sum + p.preferred, 0);
  }
  if (close !== undefined) {
    close.preferred = Math.min(closeMax, Math.max(closeMin, TARGET_SECONDS - bodySeconds));
  }

  const segments: Segment[] = [];
  let from = 0;
  for (const p of planned) {
    const duration = Math.max(1, Math.round(p.preferred * fps));
    segments.push({ ...p.spec, from, duration, rec: p.rec });
    from += duration;
  }
  // Rounding to frames can leave a frame or two; the close card takes it so the total is exact when it can be.
  const last = segments[segments.length - 1];
  const target = Math.round(TARGET_SECONDS * fps);
  if (last !== undefined && last.id === 'close' && from !== target && last.duration + (target - from) >= Math.round(closeMin * fps)) {
    last.duration += target - from;
    from = target;
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
