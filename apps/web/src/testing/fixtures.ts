// Builders for the generated messages the stubbed API answers with and the realtime frames the fake client emits. Test
// support: nothing in the app imports this file.
import { create, type DescMessage, type MessageInitShape, type MessageShape } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import {
  ArtifactSchema,
  AuthorKind,
  ChannelKind,
  ChannelSchema,
  FileSchema,
  FileStatus,
  GetMeResponseSchema,
  MeetingSchema,
  MeetingStatus,
  MemberRole,
  MemberSchema,
  MessageSchema,
  ServerEventSchema,
  TaskSchema,
  TaskState,
  TaskStatus,
  UserSchema,
  type Channel,
  type File,
  type GetMeResponse,
  type Meeting,
  type Member,
  type Message,
  type ServerEvent,
  type Task,
  type User,
} from '@kiwi/api-client';

/** 14:02 in Bangkok on the demo day, 2026-10-13. */
export const DAY = '2026-10-13';

/** An instant on the demo day, given as the Bangkok clock time `hh:mm`. */
export function at(clock: string, day: string = DAY): Date {
  return new Date(`${day}T${clock}:00+07:00`);
}

export const ME_ID = 'user-pim';

export function user(init: MessageInitShape<typeof UserSchema> = {}): User {
  return create(UserSchema, { id: 'user-arthit', email: 'arthit@siamriverside.co.th', name: 'Arthit K.', ...init });
}

export function me(): GetMeResponse {
  return create(GetMeResponseSchema, {
    user: user({ id: ME_ID, email: 'pim@siamriverside.co.th', name: 'Pimchanok S.' }),
    workspace: { id: 'workspace-1', name: 'Siam Riverside Trading', googleDomain: 'siamriverside.co.th' },
  });
}

export function channel(init: MessageInitShape<typeof ChannelSchema> = {}): Channel {
  return create(ChannelSchema, { id: 'ch-q4', name: 'q4-launch', kind: ChannelKind.PUBLIC, lastSeq: 5n, ...init });
}

export function member(init: MessageInitShape<typeof MemberSchema> = {}): Member {
  return create(MemberSchema, { userId: 'user-arthit', name: 'Arthit K.', role: MemberRole.MEMBER, ...init });
}

export const MEMBERS: Member[] = [
  member({ userId: ME_ID, name: 'Pimchanok S.', role: MemberRole.OWNER }),
  member({ userId: 'user-arthit', name: 'Arthit K.' }),
  member({ userId: 'user-nok', name: 'Nok' }),
];

export const USERS: User[] = [
  user({ id: ME_ID, email: 'pim@siamriverside.co.th', name: 'Pimchanok S.' }),
  user({ id: 'user-arthit', name: 'Arthit K.' }),
  user({ id: 'user-nok', email: 'nok@siamriverside.co.th', name: 'Nok' }),
];

/** What `create` takes for a message, without the already-built message form of the same union. */
type Init<Schema extends DescMessage> = Exclude<MessageInitShape<Schema>, MessageShape<Schema>>;

type MessageInit = Omit<Init<typeof MessageSchema>, 'seq' | 'createdAt'> & {
  seq: number;
  /** Bangkok clock time `hh:mm` on the demo day. */
  time?: string;
};

/** A human message in `ch-q4` by default, from Arthit K. */
export function message({ seq, time = '14:02', ...init }: MessageInit): Message {
  return create(MessageSchema, {
    id: `msg-${String(seq)}`,
    channelId: 'ch-q4',
    authorKind: AuthorKind.USER,
    authorId: 'user-arthit',
    createdAt: timestampFromDate(at(time)),
    ...init,
    seq: BigInt(seq),
  });
}

/** A message the assistant wrote. */
export function agentMessage(init: MessageInit): Message {
  return message({ authorKind: AuthorKind.AGENT, authorId: 'agent-assistant', ...init });
}

export function artifact(init: MessageInitShape<typeof ArtifactSchema>) {
  return create(ArtifactSchema, { generatedAt: timestampFromDate(at('14:31')), ...init });
}

type TaskInit = Omit<Init<typeof TaskSchema>, 'createdAt'> & { createdTime?: string };

export function task({ createdTime = '14:05', ...init }: TaskInit = {}): Task {
  return create(TaskSchema, {
    id: 'task-1',
    channelId: 'ch-q4',
    title: 'Send the quotation to ACME',
    state: TaskState.CONFIRMED,
    status: TaskStatus.TODO,
    ref: 'TSK-2035',
    createdAt: timestampFromDate(at(createdTime)),
    ...init,
  });
}

export function file(init: MessageInitShape<typeof FileSchema> = {}): File {
  return create(FileSchema, {
    id: 'file-1',
    channelId: 'ch-q4',
    name: 'warehouse-slots.csv',
    mime: 'text/csv',
    size: 2048n,
    status: FileStatus.ANALYZING,
    ...init,
  });
}

export function meeting(init: MessageInitShape<typeof MeetingSchema> = {}): Meeting {
  return create(MeetingSchema, {
    id: 'meeting-1',
    channelId: 'ch-q4',
    title: 'Q4 launch weekly sync',
    ref: 'MTG-1014',
    status: MeetingStatus.INGESTING,
    ...init,
  });
}

export function frame(event: ServerEvent['event']): ServerEvent {
  return create(ServerEventSchema, { event });
}
