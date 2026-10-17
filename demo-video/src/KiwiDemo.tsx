import { useEffect, useMemo, useState } from 'react';
import { AbsoluteFill, Sequence, continueRender, delayRender, interpolate, staticFile, useCurrentFrame, useVideoConfig } from 'remotion';
import { Captions } from './Captions';
import { Fonts } from './Fonts';
import { MeetCall } from './MeetCall';
import { Soundtrack } from './Soundtrack';
import { Placeholder, Screen } from './Screen';
import { CloseCard, TitleCard } from './cards';
import type { Manifest, SceneRec } from './manifest';
import { buildTimeline, recordingTime, type Segment } from './timeline';
import { color, font } from './theme';

export type KiwiDemoProps = {
  /** The scene marks of public/scenes.json, read by the composition's metadata; null before a recording exists. */
  scenes: SceneRec[] | null;
  /** The narration as captions under the screen. Off for the cut the avatar speaks over. */
  captions: boolean;
  /** The music bed and sound effects (low, under the HeyGen voice). */
  sound?: boolean;
};

let cached: Promise<Manifest | null> | null = null;

/** The full manifest (frames and cursor), fetched once per browser tab rather than passed through props. */
function loadManifest(): Promise<Manifest | null> {
  if (cached === null) {
    cached = fetch(staticFile('scenes.json'))
      .then((response) => (response.ok ? (response.json() as Promise<Manifest>) : null))
      .catch(() => null);
  }
  return cached;
}

export function KiwiDemo({ scenes, captions, sound = true }: KiwiDemoProps) {
  const { fps } = useVideoConfig();
  const [manifest, setManifest] = useState<Manifest | null>(null);
  const [handle] = useState(() => (scenes === null ? null : delayRender('Loading scenes.json')));
  useEffect(() => {
    if (handle === null) {
      return;
    }
    void loadManifest().then((loaded) => {
      setManifest(loaded);
      continueRender(handle);
    });
  }, [handle]);
  const { segments, total } = useMemo(() => buildTimeline(scenes, fps), [scenes, fps]);

  return (
    <AbsoluteFill style={{ background: color.primary, fontFamily: font.sans }}>
      <Fonts />
      <Backdrop />
      {segments.map((segment, index) => (
        <Sequence key={segment.id} from={segment.from} durationInFrames={segment.duration} name={segment.id}>
          {segment.kind === 'card' ? (
            segment.id === 'title' ? (
              <TitleCard />
            ) : (
              <CloseCard />
            )
          ) : segment.kind === 'call' ? (
            <CallScene segment={segment} />
          ) : (
            <ScreenScene segment={segment} previous={segments[index - 1]} manifest={manifest} />
          )}
        </Sequence>
      ))}
      {captions && <Captions segments={segments} />}
      {sound && (scenes === null || manifest !== null) && <Soundtrack segments={segments} manifest={manifest} total={total} />}
    </AbsoluteFill>
  );
}

/** The brand ground behind everything: Ink Teal, lit a little from the top left. */
function Backdrop() {
  return <AbsoluteFill style={{ background: `radial-gradient(1400px 800px at 28% -10%, ${color.railHover} 0%, ${color.primary} 55%, ${color.primaryDeep} 100%)` }} />;
}

type ScreenSceneProps = { segment: Segment; previous: Segment | undefined; manifest: Manifest | null };

/** A recorded scene: its slice of the recording, a crossfade in where the recording cuts, and its label chip. */
function ScreenScene({ segment, previous, manifest }: ScreenSceneProps) {
  const frame = useCurrentFrame();
  const { fps } = useVideoConfig();
  const clamp = { extrapolateLeft: 'clamp', extrapolateRight: 'clamp' } as const;
  const fadeIn = segment.fade ? interpolate(frame, [0, 12], [0, 1], clamp) : 1;
  const labelIn = interpolate(frame, [0, 10], [0, 1], clamp);
  const t = recordingTime(segment, frame, fps);
  const showPrevious = segment.fade && previous?.kind === 'screen' && previous.rec !== null && frame < 12;
  return (
    <AbsoluteFill>
      {manifest === null || segment.rec === null ? (
        <Placeholder label={segment.label ?? segment.id} />
      ) : (
        <>
          {showPrevious && previous?.rec !== null && previous?.rec !== undefined && <Screen manifest={manifest} t={previous.rec.t1} />}
          <Screen manifest={manifest} t={t} opacity={fadeIn} />
        </>
      )}
      {segment.label !== undefined && <SceneChip label={segment.label} />}
    </AbsoluteFill>
  );
}

/** The mocked Meet call, faded in, with its chip. */
function CallScene({ segment }: { segment: Segment }) {
  const frame = useCurrentFrame();
  const fadeIn = interpolate(frame, [0, 12], [0, 1], { extrapolateLeft: 'clamp', extrapolateRight: 'clamp' });
  return (
    <AbsoluteFill style={{ opacity: fadeIn }}>
      <MeetCall />
      {segment.label !== undefined && <SceneChip label={segment.label} />}
    </AbsoluteFill>
  );
}

/** The name of the scene in the corner under the screen. */
function SceneChip({ label }: { label: string }) {
  const frame = useCurrentFrame();
  const labelIn = interpolate(frame, [0, 10], [0, 1], { extrapolateLeft: 'clamp', extrapolateRight: 'clamp' });
  return (
    <div
      style={{
        position: 'absolute',
        left: 112,
        bottom: 38,
        display: 'flex',
        alignItems: 'center',
        gap: 12,
        padding: '8px 18px 8px 14px',
        borderRadius: 999,
        background: 'rgba(251,250,246,.14)',
        color: color.slipPaper,
        fontSize: 24,
        fontWeight: 600,
        letterSpacing: '0.02em',
        opacity: labelIn,
        transform: `translateX(${(1 - labelIn) * -10}px)`,
      }}
    >
      <span style={{ width: 10, height: 10, borderRadius: 999, background: color.presenceGreen }} />
      {label}
    </div>
  );
}
