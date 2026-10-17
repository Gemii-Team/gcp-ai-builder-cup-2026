import { Composition, staticFile } from 'remotion';
import { KiwiDemo } from './KiwiDemo';
import type { Manifest } from './manifest';
import { buildTimeline } from './timeline';
import { VIDEO } from './theme';

/** The scene marks of the recording, or null when nothing has been recorded yet. */
async function loadScenes(): Promise<Manifest['scenes'] | null> {
  try {
    const response = await fetch(staticFile('scenes.json'));
    if (!response.ok) {
      return null;
    }
    const manifest = (await response.json()) as Manifest;
    return manifest.scenes;
  } catch {
    return null;
  }
}

export function RemotionRoot() {
  return (
    <Composition
      id="KiwiDemo"
      component={KiwiDemo}
      fps={VIDEO.fps}
      width={VIDEO.width}
      height={VIDEO.height}
      durationInFrames={VIDEO.fps * 210}
      defaultProps={{ scenes: null, captions: true }}
      calculateMetadata={async ({ props }) => {
        const scenes = await loadScenes();
        const { total } = buildTimeline(scenes, VIDEO.fps);
        return { durationInFrames: total, props: { ...props, scenes } };
      }}
    />
  );
}
