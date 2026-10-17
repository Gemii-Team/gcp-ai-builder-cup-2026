import { AbsoluteFill, interpolate, spring, useCurrentFrame, useVideoConfig } from 'remotion';
import { KiwiLogo } from './KiwiLogo';
import { Frame } from './Screen';
import { color, font } from './theme';

/** Who is on the call; the speaker index matches the seeded transcript (backend/internal/seed/corpus/q4-launch-sync.json). */
const PEOPLE = [
  { name: 'Nok', initial: 'N', tint: '#3F7F78' },
  { name: 'Pim', initial: 'P', tint: '#B8A27A' },
  { name: 'Wirachit Panasit', initial: 'W', tint: '#2F6F67' },
] as const;

/** The transcript lines the call shows, in the order they are said: the seeded meeting's own words. */
const LINES: { who: number; at: string; text: string }[] = [
  { who: 0, at: '0:00:05', text: 'Okay, let’s start. Today: launch date, warehouse capacity, and who owns what.' },
  { who: 1, at: '0:00:21', text: 'Bang Na is at eighty-five percent. If orders grow twenty percent we run out of space around the 28th.' },
  { who: 0, at: '0:00:48', text: 'So we need to decide on the Samut Prakan overflow warehouse before Friday?' },
  { who: 1, at: '0:01:12', text: 'Yes. Or we move slow stock to a partner cross-dock.' },
  { who: 0, at: '0:01:40', text: 'Cross-dock first. We rent the overflow space only if pre-orders pass eighteen thousand.' },
  { who: 0, at: '0:02:30', text: 'The launch date stays the third of November — no change.' },
  { who: 1, at: '0:03:44', text: 'Someone from ops drafts the delivery promise — I don’t know who yet.' },
];

/** Microphone, camera, captions and more: plain stroke icons for the control bar. */
const CONTROLS = [
  'M12 3a3 3 0 0 0-3 3v6a3 3 0 0 0 6 0V6a3 3 0 0 0-3-3z M5 11a7 7 0 0 0 14 0 M12 18v3',
  'M3 7h12v10H3z M15 10l6-3v10l-6-3',
  'M3 5h18v14H3z M10 10.5a2 2 0 1 0 0 3 M16 10.5a2 2 0 1 0 0 3',
  'M6 12h.01 M12 12h.01 M18 12h.01',
];

const clamp = { extrapolateLeft: 'clamp', extrapolateRight: 'clamp' } as const;

/** A mocked video call for the meeting scene: the people, who is speaking, and the transcript K.I.W.I. captures live. */
export function MeetCall() {
  const frame = useCurrentFrame();
  const { fps, durationInFrames } = useVideoConfig();
  const endAt = durationInFrames - Math.round(fps * 3.2);
  const step = Math.floor((endAt - fps * 0.8) / LINES.length);
  const shown = Math.max(0, Math.min(LINES.length, Math.floor((frame - fps * 0.8) / step) + 1));
  const speaking = frame < endAt && shown > 0 ? LINES[shown - 1]!.who : -1;
  const seconds = 222 + Math.floor(frame / fps);
  const timer = `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
  const ended = frame >= endAt;
  const endFade = interpolate(frame, [endAt, endAt + 12], [0, 1], clamp);

  return (
    <Frame>
      <AbsoluteFill style={{ background: '#0E1B1A', fontFamily: font.sans, color: color.slipPaper }}>
        {/* Top bar */}
        <div style={{ position: 'absolute', left: 40, right: 40, top: 28, display: 'flex', alignItems: 'center', gap: 18, fontSize: 28 }}>
          <span style={{ fontWeight: 600 }}>Q4 launch sync</span>
          <span style={{ color: color.railSage }}>· #q4-launch · Google Meet</span>
          <span style={{ marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: 12, padding: '8px 18px', borderRadius: 999, background: 'rgba(95,211,160,.14)', color: color.presenceGreen, fontWeight: 600 }}>
            <span style={{ width: 12, height: 12, borderRadius: 99, background: color.presenceGreen, opacity: 0.5 + 0.5 * Math.abs(Math.sin(frame / 9)) }} />
            K.I.W.I. is transcribing
          </span>
          <span style={{ fontFamily: font.mono, color: color.railSage }}>{timer}</span>
        </div>

        {/* Tiles */}
        <div style={{ position: 'absolute', left: 40, top: 100, width: 1180, height: 820, display: 'grid', gridTemplateColumns: '1fr 1fr', gridTemplateRows: '1fr 1fr', gap: 20 }}>
          {PEOPLE.map((person, index) => (
            <Tile key={person.name} person={person} active={index === speaking} frame={frame} wide={index === 2} />
          ))}
        </div>

        {/* Live transcript */}
        <div style={{ position: 'absolute', left: 1250, right: 40, top: 100, height: 820, borderRadius: 18, background: '#152725', border: '1px solid #24544F', overflow: 'hidden' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 14, padding: '22px 26px', borderBottom: '1px solid #24544F' }}>
            <KiwiLogo size={40} />
            <div>
              <div style={{ fontSize: 26, fontWeight: 600 }}>Live transcript</div>
              <div style={{ fontSize: 20, color: color.railSage }}>Captured by K.I.W.I.</div>
            </div>
          </div>
          <div style={{ padding: '18px 26px', display: 'flex', flexDirection: 'column', gap: 18 }}>
            {LINES.slice(Math.max(0, shown - 5), shown).map((line, i, list) => {
              const born = (LINES.indexOf(line) * step + fps * 0.8) | 0;
              const p = interpolate(frame, [born, born + 10], [0, 1], clamp);
              const latest = i === list.length - 1;
              return (
                <div key={line.at} style={{ opacity: p * (latest ? 1 : 0.72), transform: `translateY(${(1 - p) * 12}px)` }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 20, color: color.railSage }}>
                    <span style={{ fontWeight: 600, color: PEOPLE[line.who]!.tint === '#B8A27A' ? '#E2CFA6' : color.tallyMint }}>{PEOPLE[line.who]!.name}</span>
                    <span style={{ fontFamily: font.mono }}>{line.at}</span>
                  </div>
                  <div style={{ fontSize: 23, lineHeight: 1.45, marginTop: 4 }}>{line.text}</div>
                </div>
              );
            })}
          </div>
        </div>

        {/* Controls */}
        <div style={{ position: 'absolute', left: 0, width: 1260, bottom: 26, display: 'flex', justifyContent: 'center', gap: 22 }}>
          {CONTROLS.map((path, i) => (
            <span key={i} style={{ width: 70, height: 70, borderRadius: 99, background: '#24544F', display: 'grid', placeItems: 'center' }}>
              <svg viewBox="0 0 24 24" width={30} height={30} fill="none" stroke={color.slipPaper} strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round">
                <path d={path} />
              </svg>
            </span>
          ))}
          <span
            style={{
              width: 110,
              height: 70,
              borderRadius: 99,
              background: '#C8402F',
              display: 'grid',
              placeItems: 'center',
              fontSize: 22,
              fontWeight: 600,
              transform: `scale(${frame >= endAt - 10 && frame < endAt + 4 ? 0.9 : 1})`,
              boxShadow: frame >= endAt - 10 && frame < endAt + 20 ? '0 0 0 8px rgba(200,64,47,.35)' : 'none',
            }}
          >
            Leave
          </span>
        </div>

        {/* The call ends: K.I.W.I. takes over */}
        {ended && (
          <AbsoluteFill style={{ background: `rgba(10,45,43,${0.88 * endFade})`, alignItems: 'center', justifyContent: 'center', textAlign: 'center' }}>
            <div style={{ opacity: endFade, transform: `translateY(${(1 - endFade) * 14}px)` }}>
              <div style={{ display: 'inline-block', transform: `scale(${0.8 + 0.2 * spring({ frame: frame - endAt, fps, config: { damping: 14 } })})` }}>
                <KiwiLogo size={96} />
              </div>
              <div style={{ marginTop: 22, fontSize: 46, fontWeight: 700 }}>Call ended</div>
              <div style={{ marginTop: 8, fontSize: 30, color: color.railSage }}>
                K.I.W.I. is writing the minutes<span style={{ opacity: frame % 30 < 15 ? 1 : 0.3 }}>…</span>
              </div>
            </div>
          </AbsoluteFill>
        )}
      </AbsoluteFill>
    </Frame>
  );
}

function Tile({ person, active, frame, wide }: { person: (typeof PEOPLE)[number]; active: boolean; frame: number; wide: boolean }) {
  return (
    <div
      style={{
        position: 'relative',
        gridColumn: wide ? '1 / span 2' : undefined,
        borderRadius: 18,
        background: '#1B3330',
        border: `3px solid ${active ? color.presenceGreen : 'transparent'}`,
        boxShadow: active ? '0 0 0 6px rgba(95,211,160,.18)' : 'none',
        display: 'grid',
        placeItems: 'center',
        transition: 'none',
      }}
    >
      <div style={{ width: 150, height: 150, borderRadius: 999, background: person.tint, display: 'grid', placeItems: 'center', fontSize: 64, fontWeight: 700, transform: `scale(${active ? 1 + 0.03 * Math.sin(frame / 3) : 1})` }}>
        {person.initial}
      </div>
      <div style={{ position: 'absolute', left: 20, bottom: 16, display: 'flex', alignItems: 'center', gap: 10, fontSize: 24, fontWeight: 600, padding: '6px 14px', borderRadius: 10, background: 'rgba(0,0,0,.35)' }}>
        {active && <Bars frame={frame} />}
        {person.name}
      </div>
    </div>
  );
}

/** The little level meter beside the name of whoever is speaking. */
function Bars({ frame }: { frame: number }) {
  return (
    <span style={{ display: 'inline-flex', alignItems: 'flex-end', gap: 3, height: 20 }}>
      {[0, 1, 2].map((i) => (
        <span key={i} style={{ width: 5, borderRadius: 2, background: color.presenceGreen, height: 6 + 14 * Math.abs(Math.sin(frame / (3 + i) + i)) }} />
      ))}
    </span>
  );
}
