import { AbsoluteFill, interpolate, spring, useCurrentFrame, useVideoConfig } from 'remotion';
import { KiwiLogo } from './KiwiLogo';
import { color, font } from './theme';

/** A value that rises from 0 to 1 over `length` frames starting at `at`, clamped. */
const rise = (frame: number, at: number, length = 14) => interpolate(frame, [at, at + length], [0, 1], { extrapolateLeft: 'clamp', extrapolateRight: 'clamp' });

function Fade({ children, inFrames = 0, outFrames = 12 }: { children: React.ReactNode; inFrames?: number; outFrames?: number }) {
  const frame = useCurrentFrame();
  const { durationInFrames } = useVideoConfig();
  const opacity = Math.min(inFrames === 0 ? 1 : rise(frame, 0, inFrames), interpolate(frame, [durationInFrames - outFrames, durationInFrames], [1, 0], { extrapolateLeft: 'clamp', extrapolateRight: 'clamp' }));
  return <AbsoluteFill style={{ opacity }}>{children}</AbsoluteFill>;
}

function Chip({ children, at }: { children: React.ReactNode; at: number }) {
  const frame = useCurrentFrame();
  const p = rise(frame, at, 16);
  return (
    <span
      style={{
        display: 'inline-block',
        padding: '10px 22px',
        borderRadius: 999,
        border: `1.5px solid ${color.railSage}`,
        color: color.slipPaper,
        fontSize: 26,
        fontWeight: 500,
        opacity: p,
        transform: `translateY(${(1 - p) * 14}px)`,
      }}
    >
      {children}
    </span>
  );
}

/** The opening: the mark, the name, what it stands for, and the four things it does. */
export function TitleCard() {
  const frame = useCurrentFrame();
  const { fps } = useVideoConfig();
  const pop = spring({ frame, fps, config: { damping: 14, stiffness: 120, mass: 0.8 } });
  return (
    <Fade>
      <AbsoluteFill style={{ alignItems: 'center', justifyContent: 'center', color: color.slipPaper, fontFamily: font.sans, textAlign: 'center' }}>
        <div style={{ transform: `scale(${0.6 + 0.4 * pop})`, opacity: pop }}>
          <KiwiLogo size={168} />
        </div>
        <div style={{ marginTop: 28, fontSize: 112, fontWeight: 700, letterSpacing: '0.06em', lineHeight: 1.1, opacity: rise(frame, 8) }}>K.I.W.I.</div>
        <div style={{ marginTop: 10, fontSize: 38, fontWeight: 500, color: color.railSage, opacity: rise(frame, 16) }}>Knowledge, Intelligence &amp; Workflow Integration</div>
        <div style={{ marginTop: 44, fontSize: 40, fontWeight: 500, maxWidth: 1300, lineHeight: 1.4, opacity: rise(frame, 40) }}>
          The AI workspace where conversations become decisions —<br />and decisions become action.
        </div>
        <div style={{ marginTop: 56, display: 'flex', gap: 18, flexWrap: 'wrap', justifyContent: 'center', maxWidth: 1500 }}>
          <Chip at={75}>AI meeting summaries</Chip>
          <Chip at={85}>Human-in-the-loop tasks</Chip>
          <Chip at={95}>Cited answers · English &amp; Thai</Chip>
          <Chip at={105}>Real-time chat · Calendar · Meet</Chip>
        </div>
        <div style={{ marginTop: 54, fontSize: 24, color: color.railSage, letterSpacing: '0.04em', opacity: rise(frame, 120) }}>GCP AI Builder Cup 2026 · Built on Google Cloud with Gemini</div>
      </AbsoluteFill>
    </Fade>
  );
}

/** The close: where it runs, what it is, thank you. */
export function CloseCard() {
  const frame = useCurrentFrame();
  const { fps } = useVideoConfig();
  const pop = spring({ frame: Math.max(0, frame - 6), fps, config: { damping: 16, stiffness: 110 } });
  return (
    <Fade inFrames={14} outFrames={20}>
      <AbsoluteFill style={{ alignItems: 'center', justifyContent: 'center', color: color.slipPaper, fontFamily: font.sans, textAlign: 'center' }}>
        <div style={{ transform: `scale(${0.7 + 0.3 * pop})`, opacity: pop }}>
          <KiwiLogo size={132} />
        </div>
        <div style={{ marginTop: 24, fontSize: 88, fontWeight: 700, letterSpacing: '0.06em', opacity: rise(frame, 14) }}>K.I.W.I.</div>
        <div style={{ marginTop: 30, fontSize: 36, fontWeight: 500, maxWidth: 1300, lineHeight: 1.45, opacity: rise(frame, 40) }}>
          One AI workspace for chat, meetings, knowledge and tasks.
          <div style={{ fontSize: 30, color: color.railSage, marginTop: 10, fontWeight: 400 }}>Every conversation, turned into action.</div>
        </div>
        <div style={{ marginTop: 56, display: 'flex', gap: 18, justifyContent: 'center' }}>
          <Chip at={70}>Built on Google Cloud</Chip>
          <Chip at={80}>Powered by Gemini</Chip>
          <Chip at={90}>GCP AI Builder Cup 2026</Chip>
        </div>
        <div style={{ marginTop: 70, fontSize: 44, fontWeight: 600, opacity: rise(frame, Math.round(fps * 17)) }}>
          Thank you
        </div>
      </AbsoluteFill>
    </Fade>
  );
}
