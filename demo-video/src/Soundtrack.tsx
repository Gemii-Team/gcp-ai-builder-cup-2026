import { Audio, Sequence, interpolate, staticFile, useVideoConfig } from 'remotion';
import type { Manifest } from './manifest';
import { frameOfRecording, type Segment, type VoiceClips } from './timeline';

/**
 * The soundtrack under the HeyGen voice, kept low on purpose: a music bed that fades in and out, a click on every press
 * of the pointer, a whoosh at each change of scene, and a chime when an AI result lands (the minutes, an answer, a
 * confirmed task, a scheduled meeting). All of it is synthesized by scripts/make-audio.py.
 */
/** The music's level on its own and while someone speaks, and how many frames the dip takes to ease in and out. */
const MUSIC = { level: 0.16, ducked: 0.05, ease: 12 } as const;

export function Soundtrack({ segments, manifest, total, voice = {} }: { segments: readonly Segment[]; manifest: Manifest | null; total: number; voice?: VoiceClips }) {
  const { fps } = useVideoConfig();
  const cues: { at: number; src: string; volume: number }[] = [];
  // Where a voice clip plays, as frame windows: the music dips under each.
  const spoken = segments.flatMap((segment) => {
    const clip = voice[segment.id];
    return clip === undefined ? [] : [{ from: segment.from, to: segment.from + Math.round(clip.seconds * fps) }];
  });
  const duck = (f: number) => {
    let amount = 0;
    for (const w of spoken) {
      amount = Math.max(amount, interpolate(f, [w.from - MUSIC.ease, w.from, w.to, w.to + MUSIC.ease], [0, 1, 1, 0], { extrapolateLeft: 'clamp', extrapolateRight: 'clamp' }));
    }
    return MUSIC.level + (MUSIC.ducked - MUSIC.level) * amount;
  };

  segments.forEach((segment, index) => {
    if (index > 0) {
      cues.push({ at: Math.max(0, segment.from - 6), src: 'audio/whoosh.wav', volume: 0.22 });
    }
    if (segment.kind === 'call') {
      // The call ends (see MeetCall: 3.2 s before the scene does).
      cues.push({ at: segment.from + segment.duration - Math.round(fps * 3.2), src: 'audio/chime.wav', volume: 0.3 });
    }
    if (manifest === null || segment.rec === null) {
      return;
    }
    for (const press of manifest.cursor) {
      if (press.k !== 'd') {
        continue;
      }
      const local = frameOfRecording(segment, press.t, fps);
      if (local !== null) {
        cues.push({ at: segment.from + local, src: 'audio/click.wav', volume: 0.35 });
      }
    }
    for (const event of manifest.events ?? []) {
      const local = frameOfRecording(segment, event.t, fps);
      if (local !== null) {
        cues.push({ at: segment.from + local, src: 'audio/chime.wav', volume: 0.3 });
      }
    }
  });

  return (
    <>
      <Audio
        src={staticFile('audio/music.wav')}
        loop
        volume={(f) => duck(f) * interpolate(f, [0, fps * 2, total - fps * 4, total], [0, 1, 1, 0], { extrapolateLeft: 'clamp', extrapolateRight: 'clamp' })}
      />
      {cues.map((cue, i) => (
        <Sequence key={i} from={cue.at} durationInFrames={Math.round(fps * 2)} layout="none" name={cue.src.replace('audio/', '')}>
          <Audio src={staticFile(cue.src)} volume={cue.volume} />
        </Sequence>
      ))}
    </>
  );
}
