# K.I.W.I. demo video

The 3½-minute screen demo of the narration script (16 Oct 2026), made from the running app rather than by hand:
a Playwright script walks the shot list on `http://localhost:8080` and captures the screen, and a Remotion
composition turns the capture into the video with a title card, scene chips, the narration as captions, a drawn
pointer and a close card. Lay the HeyGen avatar over the rendered file; render it without captions for that cut.

## Once

```bash
cd demo-video
npm install
npx playwright install chromium      # the Chromium the recorder drives (one download)
npm run login                        # opens a window: sign in with your Google account; the session is saved to auth/state.json
```

The stack must be up (`task local:up`, overlay `local` for real Google sign-in) with your address seeded
(`task seed -- --members you@gmail.com`). `auth/state.json` is git-ignored; run `npm run login` again when the session has expired.

## Each take

```bash
npm run take -- --no-warmup          # resets the Team workspace, records (retries up to 3x), renders out/kiwi-demo.mp4
npm run studio                       # Remotion Studio, to scrub through the result
```

`npm run record` and `npm run render` do the two halves on their own. NARRATION.md is the voice-over for HeyGen.

What is simulated rather than real: Google sign-in (the take cuts from the sign-in page to the workspace), Start meeting
(the RPC is answered by the recorder and no Meet tab opens), the call itself (a drawn scene, `src/MeetCall.tsx`, using
the seeded transcript), the minutes arriving (the seeded summary slip is hidden until the call ends, then revealed), and
Schedule meeting (answered by the recorder unless `--real-google`). Chat, the meeting briefing, Accept, the Board and Ask
are the real app; Ask is answered live by Gemini through `llm/`.

The soundtrack (`public/audio`, made by `python3 -I scripts/make-audio.py`) is a quiet music bed with clicks, whooshes and
chimes, kept low under the voice. Render without it with `--props='{"sound":false}'`.

## Voice-over without HeyGen

```bash
npm run voice                        # one clip per scene into public/voice/*.wav (Gemini TTS, the Mac's voice as fallback)
npm run render
```

It reads the narration from `src/script.ts`, voices it with Gemini text-to-speech (key in `llm/.env`; it moves to the next
TTS model when one is out of free quota) and falls back to the Mac's Samantha voice. Existing clips are kept; `npm run voice
-- --force` redoes them, `-- --voice Kore` picks another Gemini voice. Recorded scenes may play up to 1.5x faster so the
voiced video still lands on 4:00.

## Adding the HeyGen voice

Make one clip per scene from NARRATION.md and drop it in `public/voice/` named after the scene (`title.mp4`,
`signin.mp4`, … `close.mp4`; NARRATION.md lists each name). A video clip puts the avatar in the bottom-right corner
(rounded, 300×300, cropped to fill); an `.mp3`, `.wav` or `.m4a` is voice only. Then `npm run render`. Each clip starts
with its scene, the music dips under it, and a scene whose clip runs longer than it grows to fit, with the other scenes
giving back spare time so the video stays 4:00 (`TARGET_SECONDS` in `src/script.ts`). Any scene may have no clip. The
clips are git-ignored.

Flags for `npm run record -- <flags>`:

| Flag | Effect |
|---|---|
| `--no-schedule` | Skips the Calendar form. Without it the take schedules "Launch rehearsal" (next week, 14:00, 45 min, workspace-wide), which creates a real Google Calendar event with a Meet link on your account. |
| `--no-warmup` | Skips the unrecorded Ask question that warms the model up before the recorded one. |
| `--thai` | Also asks a Thai question after the English one (another model call; the scene runs longer). |
| `--headed` | Shows the browser while it records. |
| `--probe` | Records only the sign-in page for a few seconds (no session needed), to check the pipeline. |

What a take does, in order: the sign-in page and a press on Continue with Google (the cut to the workspace stands in for
Google's own pages); the rail's four areas; `#q4-launch` with a message in two languages and one sent offline; Meetings,
"Q4 launch sync", its summary, and Accept on the first draft task that has an owner; the Board lane of that meeting; Ask with
"What did we decide about the warehouse?" and a press on citation 1; the Calendar's three views and the schedule form.
The take posts two messages in `#q4-launch`, confirms one draft and (unless `--no-schedule`) adds one event. The seed's
draft tasks are consumed by confirming them: to show Accept again, a draft has to exist (a new recording upload, or a reseed).

To render without captions (for the avatar cut):

```bash
npx remotion render src/index.ts KiwiDemo out/kiwi-demo-clean.mp4 --codec h264 --crf 16 --props='{"captions":false}'
```

## How it fits together

- `scripts/record.ts`: the walkthrough. A CDP screencast (`Page.startScreencast`) saves every changed frame as JPEG at
  the viewport size (1920x1080, rendered at 2x, page zoom 1.15); the pointer's path, presses and the scene marks are
  stamped with the same clock into `public/scenes.json`. Timings inside a scene (`sleep`) are what sets the pace.
- `src/script.ts`: the scenes with their narration and script seconds. A recorded scene that runs longer keeps its
  length; a shorter one holds a frame. Edit the lines to change the captions.
- `src/timeline.ts`: lays the scenes out; `src/KiwiDemo.tsx` plays them (`Screen` draws the frame nearest before the
  time and the pointer from the path); `src/cards.tsx` is the opening and the close; `src/Captions.tsx` the narration.
- The fonts are the app's own (Anuphan, Noto Sans Mono) from `public/fonts`; the colours are `theme.css`'s.

The frames and the manifest are not committed (`.gitignore`); a take is reproducible from the seeded workspace.
