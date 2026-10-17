import type { Source, Track } from '@kiwi/ui';

// What the screens hold and the query cache stores: the generated messages with every int64 as a `number`, every
// timestamp as an ISO string and every enum as a short string. Nothing here is a `bigint` or a protobuf message, so a
// cache entry can be read, compared and cloned like plain data. `mappers.ts` is the one place that builds them.

export type UserView = { id: string; email: string; name: string; avatarUrl: string };

export type WorkspaceView = { id: string; name: string; googleDomain: string };

/** What `IdentityService.GetMe` answers: the signed-in user and their workspace. */
export type SessionView = { user: UserView; workspace: WorkspaceView };

export type ChannelView = { id: string; name: string; kind: 'public' | 'private'; lastSeq: number };

export type MemberView = { userId: string; name: string; avatarUrl: string; role: 'member' | 'owner' };

export type MentionView = { kind: 'user' | 'agent'; id: string; label: string };

export type FileRefView = { fileId: string; name: string; mime: string; size: number };

export type CitationView = { n: number; chunkId: string; quote: string; source: Source };

export type ArtifactView = {
  kind: 'meeting_summary' | 'doc_summary' | 'answer' | 'not_found' | 'channel_summary';
  /** The REF code without its label: `MTG-1014`. */
  ref: string;
  title: string;
  meta: string;
  bodyMarkdown: string;
  tracks: Track[];
  sources: Source[];
  citations: CitationView[];
  scopeChannelIds: string[];
  generatedAt: string;
  meetingId?: string;
  fileId?: string;
  runId?: string;
};

/** What `AgentService` says about an Ask run. `answer` is an `answer`, `not_found` or `channel_summary` artifact; a run that failed has none and no REF. */
export type RunView = {
  id: string;
  status: 'queued' | 'running' | 'done' | 'failed';
  question: string;
  channelId: string | null;
  answer: ArtifactView | null;
  /** The server's code for why the run failed (`model_unavailable`), empty unless it did. */
  failureReason: string;
};

/** One `AgentRunProgress` frame. Streamed text is ephemeral and never shown; a `done` frame carries the validated answer. */
export type RunProgressView = {
  runId: string;
  state: 'running' | 'streaming' | 'done' | 'failed';
  textDelta: string;
  answer: ArtifactView | null;
};

/**
 * `sending`: the request is out. `queued`: the network refused it and the pending queue holds it until the connection
 * is back. `failed`: the server refused it, so it will not be retried by itself. A message the server has stored has none.
 */
export type Delivery = 'sending' | 'queued' | 'failed';

export type ChatMessage = {
  id: string;
  channelId: string;
  /** The channel's gapless sequence number; 0 for a message not yet stored. */
  seq: number;
  authorKind: 'user' | 'agent';
  authorId: string;
  body: string;
  mentions: MentionView[];
  clientMsgId: string;
  /** ISO 8601. */
  createdAt: string;
  files: FileRefView[];
  artifact?: ArtifactView;
  delivery?: Delivery;
};

export type TaskView = {
  id: string;
  channelId: string;
  title: string;
  description: string;
  state: 'draft' | 'confirmed';
  status: 'todo' | 'doing' | 'done';
  assigneeId: string | null;
  dueAt: string | null;
  doneAt: string | null;
  createdAt: string;
  position: string;
  /** The `TSK-...` REF code without its label. */
  ref: string;
  /** What the task was made from; null for a task a person added by hand. */
  source: Source | null;
  /** The REF of the answer a task was made from, `ANS-2209`. */
  fromRef: string | null;
  createdByKind: 'user' | 'agent';
  createdById: string;
};

/**
 * A recording this session uploaded into a channel, and the meeting it became. The server posts no message for a recording
 * (the summary is posted when the meeting is done), so this is what the wait for it, and its failure, follow.
 */
export type RecordingUpload = {
  fileId: string;
  name: string;
  size: number;
  meetingId: string;
  /** When it was uploaded, ISO 8601. */
  at: string;
};

export type FileView = {
  id: string;
  channelId: string;
  messageId: string;
  name: string;
  mime: string;
  size: number;
  status: 'uploaded' | 'analyzing' | 'ready' | 'failed';
  /** A short code such as `too_large`, `unsupported_type` or `empty` when the file failed. */
  failureReason: string;
  ref: string;
  /** 0 when unknown. */
  pages: number;
  /** When this client learned the status changed: set by a realtime event, never by a fetch. */
  settledAt?: string;
};

/** A decision or open question the AI drew from a meeting, with the transcript lines it came from. */
export type SummaryItemView = { text: string; entryRefs: number[] };

export type MeetingView = {
  id: string;
  channelId: string;
  title: string;
  ref: string;
  status: 'pending' | 'ingesting' | 'ready' | 'failed' | 'no_transcript';
  failureReason: string;
  meetUri: string;
  startedAt: string | null;
  endedAt: string | null;
  /** The AI summary. Empty until the meeting is ready; the list (ListMeetings) carries none, GetMeeting does. */
  summary: string;
  decisions: SummaryItemView[];
  openQuestions: SummaryItemView[];
  /** When this client learned the status changed: set by a realtime event, never by a fetch. */
  settledAt?: string;
};

/**
 * A scheduled meeting (a Google Calendar event with a Meet link) the Calendar screen shows. `orgWide` events belong to
 * the whole workspace; the rest to their channel (team). `ref` is the `EVT-...` code without its label.
 */
export type CalendarEventView = {
  id: string;
  channelId: string;
  ref: string;
  title: string;
  /** ISO 8601. */
  startAt: string;
  /** ISO 8601, or null when the event carries no end. */
  endAt: string | null;
  meetUri: string;
  htmlLink: string;
  status: 'scheduled' | 'canceled';
  orgWide: boolean;
};
