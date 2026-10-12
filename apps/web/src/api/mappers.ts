import type { Timestamp } from '@bufbuild/protobuf/wkt';
import { timestampDate } from '@bufbuild/protobuf/wkt';
import {
  AgentRunState,
  ArtifactKind,
  AuthorKind,
  ChannelKind,
  CreatedByKind,
  FileStatus,
  MeetingStatus,
  MemberRole,
  MentionKind,
  RunStatus,
  SourceKind,
  TaskState,
  TaskStatus,
  TrackKind,
  type AgentRunProgress,
  type Artifact,
  type Channel,
  type File,
  type GetMeResponse,
  type Meeting,
  type Member,
  type Message,
  type Run,
  type Source as ApiSource,
  type Task,
  type Track as ApiTrack,
  type User,
} from '@kiwi/api-client';
import { toSafeNumber } from '@kiwi/core';
import type { Source, Track } from '@kiwi/ui';
import type {
  ArtifactView,
  ChannelView,
  ChatMessage,
  FileView,
  MeetingView,
  MemberView,
  RunProgressView,
  RunView,
  SessionView,
  TaskView,
  UserView,
} from './types';

// The int64 boundary: `seq`, `offset_ms`, `size` and `last_seq` arrive as `bigint` and leave as `number`, through
// `toSafeNumber`, which throws when a value would lose precision. The `@kiwi/ui` components only ever see these results.

/** An instant as ISO 8601, or null when the message carries none. */
function iso(ts: Timestamp | undefined): string | null {
  return ts === undefined ? null : timestampDate(ts).toISOString();
}

/** A message always has a creation time; the epoch stands in for a malformed one so a row still renders. */
function isoOrEpoch(ts: Timestamp | undefined): string {
  return iso(ts) ?? new Date(0).toISOString();
}

/** The number in a source anchor: a meeting's `<entry idx>` or a message's `<seq>`. */
function anchorNumber(anchor: string): number | null {
  return /^\d+$/.test(anchor) ? Number(anchor) : null;
}

/** The page in a file source's `page:<n>` anchor. */
function anchorPage(anchor: string): number | undefined {
  const match = /^page:(\d+)$/.exec(anchor);
  return match === null ? undefined : Number(match[1]);
}

export function mapUser(user: User): UserView {
  return { id: user.id, email: user.email, name: user.name, avatarUrl: user.avatarUrl };
}

export function mapSession(response: GetMeResponse): SessionView {
  const { user, workspace } = response;
  if (user === undefined || workspace === undefined) {
    throw new Error('GetMe answered without a user or a workspace.');
  }
  return { user: mapUser(user), workspace: { id: workspace.id, name: workspace.name, googleDomain: workspace.googleDomain } };
}

export function mapChannel(channel: Channel): ChannelView {
  return {
    id: channel.id,
    name: channel.name,
    kind: channel.kind === ChannelKind.PRIVATE ? 'private' : 'public',
    lastSeq: toSafeNumber(channel.lastSeq),
  };
}

export function mapMember(member: Member): MemberView {
  return {
    userId: member.userId,
    name: member.name,
    avatarUrl: member.avatarUrl,
    role: member.role === MemberRole.OWNER ? 'owner' : 'member',
  };
}

/**
 * A source as the slips show it. A transcript offset is a live link only when the excerpt behind it can be loaded (the
 * source has its entry index), so a source without timing never renders a dead `0:00:00`. Null for an unknown kind.
 */
export function mapSource(source: ApiSource): Source | null {
  const at = iso(source.at);
  const base = {
    id: source.id,
    label: source.label,
    channelId: source.channelId,
    ...(at === null ? {} : { at }),
    ...(source.quote === undefined ? {} : { quote: source.quote }),
  };
  switch (source.kind) {
    case SourceKind.MEETING: {
      const entryIdx = anchorNumber(source.anchor);
      const offsetMs = source.offsetMs === undefined ? null : toSafeNumber(source.offsetMs);
      return { kind: 'meeting', ...base, entryIdx, offsetMs: entryIdx === null ? null : offsetMs };
    }
    case SourceKind.FILE: {
      const page = source.page ?? anchorPage(source.anchor);
      return { kind: 'file', ...base, ...(page === undefined ? {} : { page }) };
    }
    case SourceKind.MESSAGE: {
      const seq = anchorNumber(source.anchor);
      return { kind: 'message', ...base, ...(seq === null ? {} : { seq }) };
    }
    default:
      return null;
  }
}

/**
 * A track of a meeting summary. Its offset links into the meeting's transcript, so it needs the meeting's id and its own
 * entry index: without either the offset is dropped, never shown as a link that opens nothing.
 */
export function mapTrack(track: ApiTrack, meetingId: string | undefined): Track {
  const timed = meetingId !== undefined && track.offsetMs !== undefined && track.entryIdx !== undefined;
  return {
    kind: track.kind === TrackKind.OPEN_QUESTION ? 'open_question' : 'decision',
    text: track.text,
    offsetMs: timed ? toSafeNumber(track.offsetMs ?? 0) : null,
    entryIdx: timed ? (track.entryIdx ?? null) : null,
  };
}

const ARTIFACT_KINDS: Partial<Record<ArtifactKind, ArtifactView['kind']>> = {
  [ArtifactKind.MEETING_SUMMARY]: 'meeting_summary',
  [ArtifactKind.DOC_SUMMARY]: 'doc_summary',
  [ArtifactKind.ANSWER]: 'answer',
  [ArtifactKind.NOT_FOUND]: 'not_found',
  [ArtifactKind.CHANNEL_SUMMARY]: 'channel_summary',
};

/** Null for an artifact of a kind this build does not know: the message then renders as plain text. */
export function mapArtifact(artifact: Artifact): ArtifactView | null {
  const kind = ARTIFACT_KINDS[artifact.kind];
  if (kind === undefined) {
    return null;
  }
  return {
    kind,
    ref: artifact.ref,
    title: artifact.title,
    meta: artifact.meta,
    bodyMarkdown: artifact.bodyMarkdown,
    tracks: artifact.tracks.map((track) => mapTrack(track, artifact.meetingId)),
    sources: artifact.sources.flatMap((source) => mapSource(source) ?? []),
    citations: artifact.citations.flatMap((citation) => {
      const source = citation.source === undefined ? null : mapSource(citation.source);
      return source === null ? [] : [{ n: citation.n, chunkId: citation.chunkId, quote: citation.quote, source }];
    }),
    scopeChannelIds: [...artifact.scopeChannelIds],
    generatedAt: isoOrEpoch(artifact.generatedAt),
    ...(artifact.meetingId === undefined ? {} : { meetingId: artifact.meetingId }),
    ...(artifact.fileId === undefined ? {} : { fileId: artifact.fileId }),
    ...(artifact.runId === undefined ? {} : { runId: artifact.runId }),
  };
}

/**
 * The answer of an Ask run: an `answer` or a `not_found` artifact, the only two a run issues, and null for anything else
 * (no artifact, or one of a kind Ask cannot show). `ref` fills the artifact's own REF when the server left it empty.
 */
function mapAnswer(artifact: Artifact | undefined, ref: string): ArtifactView | null {
  const view = artifact === undefined ? null : mapArtifact(artifact);
  if (view === null || (view.kind !== 'answer' && view.kind !== 'not_found')) {
    return null;
  }
  return view.ref === '' ? { ...view, ref } : view;
}

const RUN_STATUSES: Partial<Record<RunStatus, RunView['status']>> = {
  [RunStatus.RUNNING]: 'running',
  [RunStatus.DONE]: 'done',
  [RunStatus.FAILED]: 'failed',
};

export function mapRun(run: Run): RunView {
  return {
    id: run.id,
    // An unspecified status is a run that has not started yet.
    status: RUN_STATUSES[run.status] ?? 'queued',
    question: run.question,
    channelId: run.channelId === undefined || run.channelId === '' ? null : run.channelId,
    answer: mapAnswer(run.answer, run.ref),
    failureReason: run.failureReason,
  };
}

const RUN_STATES: Partial<Record<AgentRunState, RunProgressView['state']>> = {
  [AgentRunState.STREAMING]: 'streaming',
  [AgentRunState.DONE]: 'done',
  [AgentRunState.FAILED]: 'failed',
};

export function mapRunProgress(progress: AgentRunProgress): RunProgressView {
  return {
    runId: progress.runId,
    // An unspecified state is tool progress, like `RUNNING`.
    state: RUN_STATES[progress.state] ?? 'running',
    textDelta: progress.textDelta,
    answer: mapAnswer(progress.answer, ''),
  };
}

export function mapMessage(message: Message): ChatMessage {
  const artifact = message.artifact === undefined ? null : mapArtifact(message.artifact);
  return {
    id: message.id,
    channelId: message.channelId,
    seq: toSafeNumber(message.seq),
    authorKind: message.authorKind === AuthorKind.AGENT ? 'agent' : 'user',
    authorId: message.authorId,
    body: message.body,
    mentions: message.mentions.map((mention) => ({
      kind: mention.kind === MentionKind.AGENT ? 'agent' : 'user',
      id: mention.id,
      label: mention.label,
    })),
    clientMsgId: message.clientMsgId,
    createdAt: isoOrEpoch(message.createdAt),
    files: message.files.map((file) => ({ fileId: file.fileId, name: file.name, mime: file.mime, size: toSafeNumber(file.size) })),
    ...(artifact === null ? {} : { artifact }),
  };
}

export function mapTask(task: Task): TaskView {
  return {
    id: task.id,
    channelId: task.channelId,
    title: task.title,
    description: task.description,
    state: task.state === TaskState.DRAFT ? 'draft' : 'confirmed',
    status: task.status === TaskStatus.DOING ? 'doing' : task.status === TaskStatus.DONE ? 'done' : 'todo',
    assigneeId: task.assigneeId ?? null,
    dueAt: iso(task.dueAt),
    doneAt: iso(task.doneAt),
    createdAt: isoOrEpoch(task.createdAt),
    position: task.position,
    ref: task.ref,
    source: task.source === undefined ? null : mapSource(task.source),
    fromRef: task.fromRef ?? null,
    createdByKind: task.createdByKind === CreatedByKind.AGENT ? 'agent' : 'user',
    createdById: task.createdById,
  };
}

const FILE_STATUSES: Partial<Record<FileStatus, FileView['status']>> = {
  [FileStatus.ANALYZING]: 'analyzing',
  [FileStatus.READY]: 'ready',
  [FileStatus.FAILED]: 'failed',
};

/** The file status an event or a response names, as the screens read it. An unknown status is a freshly `uploaded` file. */
export function mapFileStatus(status: FileStatus): FileView['status'] {
  return FILE_STATUSES[status] ?? 'uploaded';
}

export function mapFile(file: File): FileView {
  return {
    id: file.id,
    channelId: file.channelId,
    messageId: file.messageId,
    name: file.name,
    mime: file.mime,
    size: toSafeNumber(file.size),
    status: mapFileStatus(file.status),
    failureReason: file.failureReason,
    ref: file.ref,
    pages: file.pages,
  };
}

const MEETING_STATUSES: Partial<Record<MeetingStatus, MeetingView['status']>> = {
  [MeetingStatus.INGESTING]: 'ingesting',
  [MeetingStatus.READY]: 'ready',
  [MeetingStatus.FAILED]: 'failed',
  [MeetingStatus.NO_TRANSCRIPT]: 'no_transcript',
};

/** The meeting status an event or a response names, as the screens read it. An unknown status is `pending`. */
export function mapMeetingStatus(status: MeetingStatus): MeetingView['status'] {
  return MEETING_STATUSES[status] ?? 'pending';
}

export function mapMeeting(meeting: Meeting): MeetingView {
  return {
    id: meeting.id,
    channelId: meeting.channelId,
    title: meeting.title,
    ref: meeting.ref,
    status: mapMeetingStatus(meeting.status),
    failureReason: meeting.failureReason,
    meetUri: meeting.meetUri,
    startedAt: iso(meeting.startedAt),
    endedAt: iso(meeting.endedAt),
  };
}
