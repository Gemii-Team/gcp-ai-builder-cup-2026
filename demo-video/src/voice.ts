import { getAudioDurationInSeconds, getVideoMetadata } from '@remotion/media-utils';
import { getStaticFiles, staticFile } from 'remotion';
import { SCENES } from './script';
import type { VoiceClips } from './timeline';

/** The file names a scene's clip may have under public/voice/, in the order they are looked for. */
const KINDS = [
  { ext: 'mp4', video: true },
  { ext: 'mov', video: true },
  { ext: 'webm', video: true },
  { ext: 'mp3', video: false },
  { ext: 'wav', video: false },
  { ext: 'm4a', video: false },
] as const;


/**
 * The HeyGen clips that are present: public/voice/<scene id>.<mp4|mov|webm|mp3|wav|m4a>, one per scene (title, signin,
 * workspace, chat, meet, call, mom, meetings, board, ask, calendar, close). A video clip shows the avatar in the corner;
 * an audio clip is voice only. Any scene may have none.
 */
export async function loadVoice(): Promise<VoiceClips> {
  const clips: VoiceClips = {};
  // The files Remotion serves from public/ (listed when the project is bundled), so absent clips are not probed for.
  const present = new Set(getStaticFiles().map((file) => file.name));
  await Promise.all(
    SCENES.map(async (scene) => {
      for (const kind of KINDS) {
        const file = `voice/${scene.id}.${kind.ext}`;
        if (!present.has(file)) {
          continue;
        }
        const seconds = kind.video ? (await getVideoMetadata(staticFile(file))).durationInSeconds : await getAudioDurationInSeconds(staticFile(file));
        clips[scene.id] = { file, seconds, video: kind.video };
        return;
      }
    }),
  );
  return clips;
}
