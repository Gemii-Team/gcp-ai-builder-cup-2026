import { Img, staticFile } from 'remotion';
import { CursorLayer } from './Cursor';
import { cursorAt, frameAt, pressesNear, type Manifest } from './manifest';
import { color, VIDEO } from './theme';

/** Where the captured screen sits in the frame: a little smaller than full bleed, with room below for the captions. */
export const SCREEN_BOX = { x: 112, y: 34, w: 1696, h: 954 } as const;

type ScreenProps = {
  manifest: Manifest;
  /** The second of the recording to show. */
  t: number;
  opacity?: number;
};

/** The captured frame nearest before `t`, scaled into the screen box, with the pointer where it was. */
export function Screen({ manifest, t, opacity = 1 }: ScreenProps) {
  const index = frameAt(manifest.frames, t);
  const frame = manifest.frames[Math.max(0, index)];
  const sx = VIDEO.width / manifest.viewport.width;
  const sy = VIDEO.height / manifest.viewport.height;
  const pointer = cursorAt(manifest.cursor, t);
  const presses = pressesNear(manifest.cursor, t, 0.45).map((press) => ({ ...press, x: press.x * sx, y: press.y * sy, age: t - press.t }));
  return (
    <Frame opacity={opacity}>
      {frame !== undefined && <Img src={staticFile(`frames/${frame.f}`)} style={{ width: 1920, height: 1080, display: 'block' }} />}
      <CursorLayer at={pointer === null ? null : { x: pointer.x * sx, y: pointer.y * sy }} presses={presses} />
    </Frame>
  );
}

/** The screen box: rounded, shadowed, holding a 1920x1080 stage scaled to fit. */
export function Frame({ children, opacity = 1 }: { children: React.ReactNode; opacity?: number }) {
  return (
    <div
      style={{
        position: 'absolute',
        left: SCREEN_BOX.x,
        top: SCREEN_BOX.y,
        width: SCREEN_BOX.w,
        height: SCREEN_BOX.h,
        borderRadius: 18,
        overflow: 'hidden',
        background: color.slipPaper,
        boxShadow: '0 24px 70px rgba(0,0,0,.38), 0 0 0 1px rgba(255,255,255,.08)',
        opacity,
      }}
    >
      <div style={{ position: 'absolute', left: 0, top: 0, width: 1920, height: 1080, transform: `scale(${SCREEN_BOX.w / 1920})`, transformOrigin: 'top left' }}>{children}</div>
    </div>
  );
}

/** What a screen scene shows before anything has been recorded. */
export function Placeholder({ label }: { label: string }) {
  return (
    <Frame>
      <div style={{ position: 'absolute', inset: 0, display: 'grid', placeItems: 'center', background: color.counterMint, color: color.carbonCopy, fontSize: 40, textAlign: 'center', lineHeight: 1.5 }}>
        <div>
          <div style={{ fontWeight: 600, color: color.ledgerInk }}>{label}</div>
          <div style={{ fontSize: 26 }}>No recording yet — run `npm run login`, then `npm run record`.</div>
        </div>
      </div>
    </Frame>
  );
}
