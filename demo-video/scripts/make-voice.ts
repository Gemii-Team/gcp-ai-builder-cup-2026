/**
 * Voices the narration without HeyGen: one clip per scene into public/voice/<scene>.wav, read from src/script.ts.
 * Uses Gemini text-to-speech with the key in llm/.env (trying each TTS model in turn when one is out of quota), and the
 * Mac's own voice (`say`) for any scene Gemini cannot voice. Existing clips are kept; pass --force to redo them.
 *   npx tsx scripts/make-voice.ts [--force] [--voice Charon]
 */
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { SCENES } from '../src/script';

const OUT = path.resolve('public', 'voice');
const args = process.argv.slice(2);
const FORCE = args.includes('--force');
const VOICE = args.includes('--voice') ? args[args.indexOf('--voice') + 1]! : 'Charon';
const MODELS = ['gemini-2.5-flash-preview-tts', 'gemini-3.1-flash-tts-preview', 'gemini-3.8-flash-tts', 'gemini-3.8-flash-lite-tts', 'gemini-2.5-pro-preview-tts'];
const STYLE = 'Read this as the narrator of a polished product demo video: warm, confident and clear, at a calm, unhurried pace.';

const key = fs
  .readFileSync(path.resolve('llm', '.env'), 'utf8')
  .split('\n')
  .find((line) => line.startsWith('GEMINI_API_KEY='))
  ?.slice('GEMINI_API_KEY='.length)
  .trim();

/** A 16-bit mono PCM buffer as a WAV file. */
function wav(pcm: Buffer, rate: number): Buffer {
  const header = Buffer.alloc(44);
  header.write('RIFF', 0);
  header.writeUInt32LE(36 + pcm.length, 4);
  header.write('WAVEfmt ', 8);
  header.writeUInt32LE(16, 16);
  header.writeUInt16LE(1, 20);
  header.writeUInt16LE(1, 22);
  header.writeUInt32LE(rate, 24);
  header.writeUInt32LE(rate * 2, 28);
  header.writeUInt16LE(2, 32);
  header.writeUInt16LE(16, 34);
  header.write('data', 36);
  header.writeUInt32LE(pcm.length, 40);
  return Buffer.concat([header, pcm]);
}

const exhausted = new Set<string>();

async function gemini(text: string): Promise<{ audio: Buffer; model: string } | null> {
  if (key === undefined || key === '') {
    return null;
  }
  for (const model of MODELS) {
    if (exhausted.has(model)) {
      continue;
    }
    const response = await fetch(`https://generativelanguage.googleapis.com/v1beta/models/${model}:generateContent`, {
      method: 'POST',
      headers: { 'x-goog-api-key': key, 'Content-Type': 'application/json' },
      body: JSON.stringify({
        contents: [{ parts: [{ text: `${STYLE}\n\n${text}` }] }],
        generationConfig: { responseModalities: ['AUDIO'], speechConfig: { voiceConfig: { prebuiltVoiceConfig: { voiceName: VOICE } } } },
      }),
    }).catch(() => null);
    if (response === null || !response.ok) {
      const status = response?.status ?? 'network';
      console.log(`  ${model}: ${status}, trying the next model`);
      exhausted.add(model);
      continue;
    }
    const body = (await response.json()) as { candidates?: { content?: { parts?: { inlineData?: { data: string; mimeType: string } }[] } }[] };
    const inline = body.candidates?.[0]?.content?.parts?.find((part) => part.inlineData !== undefined)?.inlineData;
    if (inline === undefined) {
      console.log(`  ${model}: no audio in the answer, trying the next model`);
      continue;
    }
    const rate = Number(/rate=(\d+)/.exec(inline.mimeType)?.[1] ?? 24000);
    return { audio: wav(Buffer.from(inline.data, 'base64'), rate), model };
  }
  return null;
}

/** The Mac's own voice, as a WAV, for when Gemini cannot. */
function mac(text: string, file: string): void {
  execFileSync('say', ['-v', 'Samantha', '-r', '170', '-o', file, '--file-format=WAVE', '--data-format=LEI16@24000', text]);
}

async function main(): Promise<void> {
  fs.mkdirSync(OUT, { recursive: true });
  for (const scene of SCENES) {
    const file = path.join(OUT, `${scene.id}.wav`);
    if (!FORCE && fs.existsSync(file)) {
      console.log(`${scene.id}: kept`);
      continue;
    }
    const text = scene.lines.join(' ').replace(/K\.I\.W\.I\./g, 'Kiwi');
    const voiced = await gemini(text);
    if (voiced !== null) {
      fs.writeFileSync(file, voiced.audio);
      console.log(`${scene.id}: Gemini (${voiced.model})`);
    } else {
      mac(text, file);
      console.log(`${scene.id}: Mac voice`);
    }
  }
}

main().catch((error: unknown) => {
  console.error(error);
  process.exit(1);
});
