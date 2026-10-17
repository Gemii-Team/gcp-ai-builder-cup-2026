import { Audio, OffthreadVideo, interpolate, staticFile, useCurrentFrame, useVideoConfig } from 'remotion';
import type { VoiceClip } from './timeline';
import { color } from './theme';

/** Where the presenter sits: the bottom-right corner, over the screen's corner and clear of the captions. */
export const AVATAR_BOX = { width: 300, height: 300, right: 40, bottom: 26 } as const;

/** One scene's HeyGen clip: the avatar in a rounded frame when the clip has a picture, and its voice either way. */
export function Avatar({ clip }: { clip: VoiceClip }) {
  const frame = useCurrentFrame();
  const { fps } = useVideoConfig();
  const end = Math.round(clip.seconds * fps);
  const opacity = interpolate(frame, [0, 8, end - 8, end], [0, 1, 1, 0], { extrapolateLeft: 'clamp', extrapolateRight: 'clamp' });
  const rise = interpolate(frame, [0, 10], [16, 0], { extrapolateLeft: 'clamp', extrapolateRight: 'clamp' });
  if (!clip.video) {
    return <Audio src={staticFile(clip.file)} />;
  }
  return (
    <div
      style={{
        position: 'absolute',
        right: AVATAR_BOX.right,
        bottom: AVATAR_BOX.bottom,
        width: AVATAR_BOX.width,
        height: AVATAR_BOX.height,
        borderRadius: 28,
        overflow: 'hidden',
        opacity,
        transform: `translateY(${rise}px)`,
        boxShadow: '0 18px 50px rgba(0,0,0,.45)',
        border: `3px solid ${color.slipPaper}`,
        background: color.primaryDeep,
      }}
    >
      <OffthreadVideo src={staticFile(clip.file)} style={{ width: '100%', height: '100%', objectFit: 'cover' }} />
    </div>
  );
}
