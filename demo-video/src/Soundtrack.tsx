import { Audio, Sequence, interpolate, staticFile, useVideoConfig } from 'remotion';
import type { Manifest } from './manifest';
import { frameOfRecording, type Segment } from './timeline';

/**
 * The soundtrack under the HeyGen voice, kept low on purpose: a music bed that fades in and out, a click on every press
 * of the pointer, a whoosh at each change of scene, and a chime when an AI result lands (the minutes, an answer, a
 * confirmed task, a scheduled meeting). All of it is synthesized by scripts/make-audio.py.
 */
export function Soundtrack({ segments, manifest, total }: { segments: readonly Segment[]; manifest: Manifest | null; total: number }) {
  const { fps } = useVideoConfig();
  const cues: { at: number; src: string; volume: number }[] = [];

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
        volume={(f) => 0.16 * interpolate(f, [0, fps * 2, total - fps * 4, total], [0, 1, 1, 0], { extrapolateLeft: 'clamp', extrapolateRight: 'clamp' })}
      />
      {cues.map((cue, i) => (
        <Sequence key={i} from={cue.at} durationInFrames={Math.round(fps * 2)} layout="none" name={cue.src.replace('audio/', '')}>
          <Audio src={staticFile(cue.src)} volume={cue.volume} />
        </Sequence>
      ))}
    </>
  );
}
