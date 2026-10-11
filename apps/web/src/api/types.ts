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
  /** When this client learned the status changed: set by a realtime event, never by a fetch. */
  settledAt?: string;
};
