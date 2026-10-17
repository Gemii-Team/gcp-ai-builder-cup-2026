/**
 * The demo's scenes, in order, with the narration of each (NARRATION.md is the same text, for HeyGen) and the seconds the
 * script gives it. A recorded scene that runs longer than its seconds keeps its own length; one that runs shorter holds a frame
 * (at its start or its end) to fill them. Edit the lines here to change the captions.
 */
export type SceneSpec = {
  id: string;
  /** A card is drawn by the composition; a screen scene plays its slice of the recording; 'call' is the mocked Meet call. */
  kind: 'card' | 'screen' | 'call';
  /** Seconds the narration script gives the scene. */
  seconds: number;
  /** How a short recording fills the seconds: holding its first or last frame still, or playing slower ('stretch'). */
  hold: 'start' | 'end' | 'stretch';
  /** A quick crossfade in from the previous picture (a cut in the recording, or a card before it). */
  fade: boolean;
  /** The chip in the corner while the scene plays. */
  label?: string;
  /** The narration, one caption at a time. */
  lines: string[];
};

/** The finished video's length: the close card grows or shrinks (within limits) so the total lands on it. */
export const TARGET_SECONDS = 240;
/** The shortest and longest the close card may be when it absorbs the difference. */
export const CLOSE_RANGE = { min: 12, max: 30 } as const;

export const SCENES: readonly SceneSpec[] = [
  {
    id: 'title',
    kind: 'card',
    seconds: 10,
    hold: 'end',
    fade: false,
    lines: ['Meet K.I.W.I. — Knowledge, Intelligence and Workflow Integration.', 'The AI workspace where conversations become decisions — and decisions become action.'],
  },
  {
    id: 'signin',
    kind: 'screen',
    seconds: 14,
    hold: 'start',
    fade: true,
    label: 'Sign in',
    lines: [
      'K.I.W.I. turns what your team says and shares into summaries, action items and answers that cite their sources — in English and Thai.',
      'And nothing the AI drafts takes effect until a person confirms it.',
    ],
  },
  {
    id: 'workspace',
    kind: 'screen',
    seconds: 15,
    hold: 'end',
    fade: true,
    label: 'Workspace',
    lines: ['Sign in with Google and you land in your team’s workspace.', 'One rail takes you to Chat, Workspace, Calendar and Agents — everything the team needs, in one place.'],
  },
  {
    id: 'chat',
    kind: 'screen',
    seconds: 18,
    hold: 'end',
    fade: false,
    label: 'Chat',
    lines: ['In a channel, your team talks in real time — in English or Thai.', 'A message sent offline waits, and arrives the moment you reconnect.'],
  },
  {
    id: 'meet',
    kind: 'screen',
    seconds: 6,
    hold: 'end',
    fade: false,
    label: 'Meeting',
    lines: ['When it’s time to talk, start a Google Meet straight from the channel.'],
  },
  {
    id: 'call',
    kind: 'call',
    seconds: 15,
    hold: 'end',
    fade: true,
    label: 'Meeting',
    lines: ['K.I.W.I. follows the call and captures every word of the transcript.', 'No one has to take notes.'],
  },
  {
    id: 'mom',
    kind: 'screen',
    seconds: 16,
    hold: 'end',
    fade: true,
    label: 'Minutes of meeting',
    lines: [
      'When the call ends, the minutes arrive in the channel —',
      'the overview, the decisions and the open questions, each linked to the moment it was said.',
    ],
  },
  {
    id: 'meetings',
    kind: 'screen',
    seconds: 36,
    hold: 'stretch',
    fade: false,
    label: 'Meetings',
    lines: [
      'Every meeting is kept in Meetings, with its full briefing.',
      'Alongside it, the AI has already drafted the action items.',
      'You review them, assign an owner and accept — and the draft becomes a real task.',
      'The AI proposes; your team decides.',
    ],
  },
  {
    id: 'board',
    kind: 'screen',
    seconds: 10,
    hold: 'end',
    fade: false,
    label: 'Board',
    lines: ['Confirmed tasks land on the Board, filed by the meeting or channel they came from.'],
  },
  {
    id: 'ask',
    kind: 'screen',
    seconds: 33,
    hold: 'end',
    fade: false,
    label: 'Ask',
    lines: [
      'When you have a question, just ask.',
      'K.I.W.I. searches your channels, meetings and files, and answers with citations you can open — in English or Thai.',
      'Every AI result carries a reference code and its sources, so you always know where it came from.',
    ],
  },
  {
    id: 'calendar',
    kind: 'screen',
    seconds: 24,
    hold: 'end',
    fade: false,
    label: 'Calendar',
    lines: [
      'The Calendar gives you Month, Week and Day views.',
      'Schedule a meeting for your team or the whole organization, and K.I.W.I. creates the event with a Meet link, ready to join.',
    ],
  },
  {
    id: 'close',
    kind: 'card',
    seconds: 22,
    hold: 'end',
    fade: true,
    lines: [
      'K.I.W.I. runs entirely on Google Cloud, powered by Gemini.',
      'Chat, meetings, knowledge and tasks — unified in one AI workspace, where every conversation turns into action.',
      'K.I.W.I. — the AI workspace for your team. Thank you.',
    ],
  },
];
