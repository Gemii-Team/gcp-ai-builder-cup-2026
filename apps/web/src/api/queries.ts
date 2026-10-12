import { queryOptions, type QueryClient } from '@tanstack/react-query';
import type { CursorStore } from '@kiwi/core';
import type { Api } from './client';
import { mapChannel, mapFile, mapMeeting, mapMember, mapMessage, mapSession, mapTask, mapUser } from './mappers';
import type { ChannelView, ChatMessage, Delivery, TaskView } from './types';

/**
 * The query-key factory: the shapes the whole app shares. Realtime events and the screens read and write the cache
 * through these, so a key is spelled once. `['tasks']` is the workspace-wide list the Board reads; `['tasks', { channelId }]`
 * is one channel's, which the channel's drafts panel reads. The first is a prefix of the second.
 */
export const keys = {
  me: () => ['me'] as const,
  /** The names `GET /api/auth/config` gives the sign-in page; a plain `fetch`, not a Connect call. */
  authConfig: () => ['auth-config'] as const,
  users: () => ['users'] as const,
  channels: () => ['channels'] as const,
  members: (channelId: string) => ['members', channelId] as const,
  messages: (channelId: string) => ['messages', channelId] as const,
  tasks: () => ['tasks'] as const,
  channelTasks: (channelId: string) => ['tasks', { channelId }] as const,
  meeting: (id: string) => ['meeting', id] as const,
  file: (id: string) => ['file', id] as const,
  /** The recordings this session uploaded into a channel (`RecordingUpload[]`): what their pending and failed rows follow. */
  recordings: (channelId: string) => ['recordings', channelId] as const,
};

/** The latest page, and every page `Load earlier` adds. */
export const MESSAGE_PAGE_SIZE = 50;

/** One channel's messages, oldest first, with whether older ones may exist. Unsent messages of this person sit last. */
export type MessagesData = { messages: ChatMessage[]; hasEarlier: boolean };

/** The one `IdentityService.GetMe` query: the session guard and the screens that show the signed-in user share it. */
export const meQuery = (api: Api) =>
  queryOptions({
    queryKey: keys.me(),
    queryFn: async ({ signal }) => mapSession(await api.identity.getMe({}, { signal })),
  });

export const usersQuery = (api: Api) =>
  queryOptions({
    queryKey: keys.users(),
    queryFn: async ({ signal }) => (await api.identity.listUsers({}, { signal })).users.map(mapUser),
  });

export const channelsQuery = (api: Api) =>
  queryOptions({
    queryKey: keys.channels(),
    queryFn: async ({ signal }) => (await api.chat.listChannels({}, { signal })).channels.map(mapChannel),
  });

export const membersQuery = (api: Api, channelId: string) =>
  queryOptions({
    queryKey: keys.members(channelId),
    queryFn: async ({ signal }) => (await api.chat.listMembers({ channelId }, { signal })).members.map(mapMember),
  });

/** Every task of the channels the caller belongs to. */
export const tasksQuery = (api: Api) =>
  queryOptions({
    queryKey: keys.tasks(),
    queryFn: async ({ signal }) => (await api.tasks.listTasks({}, { signal })).tasks.map(mapTask),
  });

export const channelTasksQuery = (api: Api, channelId: string) =>
  queryOptions({
    queryKey: keys.channelTasks(channelId),
    queryFn: async ({ signal }) => (await api.tasks.listTasks({ channelId }, { signal })).tasks.map(mapTask),
  });

export const fileQuery = (api: Api, id: string) =>
  queryOptions({
    queryKey: keys.file(id),
    queryFn: async ({ signal }) => {
      const { file } = await api.files.getFile({ fileId: id }, { signal });
      if (file === undefined) {
        throw new Error('GetFile answered without a file.');
      }
      return mapFile(file);
    },
  });

export const meetingQuery = (api: Api, id: string) =>
  queryOptions({
    queryKey: keys.meeting(id),
    queryFn: async ({ signal }) => {
      const { meeting } = await api.meetings.getMeeting({ meetingId: id }, { signal });
      if (meeting === undefined) {
        throw new Error('GetMeeting answered without a meeting.');
      }
      return mapMeeting(meeting);
    },
  });

/** One page of `ListMessages` (it pages newest first), oldest first. A full page means older messages may exist. */
export async function fetchMessagePage(
  api: Api,
  channelId: string,
  beforeSeq: number,
  signal?: AbortSignal,
): Promise<MessagesData> {
  const { messages } = await api.chat.listMessages({ channelId, beforeSeq: BigInt(beforeSeq), limit: MESSAGE_PAGE_SIZE }, { signal });
  return { messages: messages.map(mapMessage).sort((a, b) => a.seq - b.seq), hasEarlier: messages.length >= MESSAGE_PAGE_SIZE };
}

/**
 * The latest page of a channel. The cursor store learns its newest seq, so the realtime client asks only for what came
 * after it. Messages this person has not got to the server yet are kept when the query is fetched again (a refetch of this
 * query, not a reload of the page: they live only in this tab's memory, and a reload loses them).
 */
export const messagesQuery = (api: Api, cursors: CursorStore, channelId: string) =>
  queryOptions({
    queryKey: keys.messages(channelId),
    // Realtime keeps the cache current: a refetch on window focus would only race the frames it already applied.
    staleTime: Infinity,
    refetchOnWindowFocus: false,
    queryFn: async ({ client, signal }): Promise<MessagesData> => {
      const page = await fetchMessagePage(api, channelId, 0, signal);
      const newest = page.messages.at(-1);
      if (newest !== undefined) {
        cursors.advance(channelId, newest.seq);
      }
      const unsent = client.getQueryData<MessagesData>(keys.messages(channelId))?.messages.filter((m) => m.delivery !== undefined) ?? [];
      return { ...page, messages: [...page.messages, ...unsent] };
    },
  });

/** `message` among the messages: replaces the unsent row with its `client_msg_id`, ignores one it already holds, else inserts by seq. */
export function reconcileMessage(messages: ChatMessage[], message: ChatMessage): ChatMessage[] {
  const sent = messages.find((m) => m.clientMsgId !== '' && m.clientMsgId === message.clientMsgId);
  if (sent !== undefined && sent.delivery === undefined) {
    return messages;
  }
  const stored = messages.filter((m) => m.delivery === undefined);
  if (stored.some((m) => m.id === message.id)) {
    return messages;
  }
  const unsent = messages.filter((m) => m.delivery !== undefined && m.clientMsgId !== message.clientMsgId);
  const at = stored.findIndex((m) => m.seq > message.seq);
  return [...(at === -1 ? [...stored, message] : [...stored.slice(0, at), message, ...stored.slice(at)]), ...unsent];
}

/**
 * Applies a stored message to the cache: the channel's head moves (the rail derives its unread count from it) and,
 * when the channel's messages are cached, the message joins them exactly once, whether it arrives as the send's reply,
 * as a realtime frame or in a resync.
 */
export function receiveMessage(queryClient: QueryClient, message: ChatMessage): void {
  queryClient.setQueryData<ChannelView[]>(keys.channels(), (channels) => {
    const behind = channels?.some((channel) => channel.id === message.channelId && channel.lastSeq < message.seq);
    return behind === true ? channels?.map((channel) => (channel.id === message.channelId ? { ...channel, lastSeq: message.seq } : channel)) : channels;
  });
  queryClient.setQueryData<MessagesData>(keys.messages(message.channelId), (data) => {
    if (data === undefined) {
      return data;
    }
    const messages = reconcileMessage(data.messages, message);
    return messages === data.messages ? data : { ...data, messages };
  });
}

/** Shows a message of this person before the server has stored it. */
export function addUnsent(queryClient: QueryClient, message: ChatMessage): void {
  queryClient.setQueryData<MessagesData>(keys.messages(message.channelId), (data) =>
    data === undefined ? data : { ...data, messages: [...data.messages, message] },
  );
}

/** Moves an unsent message to another delivery state (`failed` once the server refuses it). */
export function markDelivery(queryClient: QueryClient, channelId: string, clientMsgId: string, delivery: Delivery): void {
  queryClient.setQueryData<MessagesData>(keys.messages(channelId), (data) =>
    data === undefined
      ? data
      : { ...data, messages: data.messages.map((m) => (m.delivery !== undefined && m.clientMsgId === clientMsgId ? { ...m, delivery } : m)) },
  );
}

/** Adds the older page that `Load earlier` fetched in front of the messages held. */
export function prependMessages(queryClient: QueryClient, channelId: string, page: MessagesData): void {
  queryClient.setQueryData<MessagesData>(keys.messages(channelId), (data) => {
    if (data === undefined) {
      return data;
    }
    const held = new Set(data.messages.map((m) => m.id));
    return { hasEarlier: page.hasEarlier, messages: [...page.messages.filter((m) => !held.has(m.id)), ...data.messages] };
  });
}

/**
 * The one place a created or changed task enters the cache, used for a task a person makes from a message, a draft that
 * was confirmed or edited, and every `task_changed` event. It replaces the task in the workspace-wide list and in its
 * channel's list when it is there and adds it at the top when it is not, so the same task never appears twice. A list
 * nobody has loaded is left alone: its first load fetches the task.
 */
export function writeTask(queryClient: QueryClient, task: TaskView): void {
  const upsert = (tasks: TaskView[] | undefined) => {
    if (tasks === undefined) {
      return tasks;
    }
    return tasks.some((t) => t.id === task.id) ? tasks.map((t) => (t.id === task.id ? task : t)) : [task, ...tasks];
  };
  queryClient.setQueryData<TaskView[]>(keys.tasks(), upsert);
  queryClient.setQueryData<TaskView[]>(keys.channelTasks(task.channelId), upsert);
}

/** Drops a channel the person was removed from: the rail loses it and nothing of it stays cached. */
export function removeChannel(queryClient: QueryClient, channelId: string): void {
  queryClient.setQueryData<ChannelView[]>(keys.channels(), (channels) => channels?.filter((channel) => channel.id !== channelId));
  queryClient.setQueryData<TaskView[]>(keys.tasks(), (tasks) => tasks?.filter((task) => task.channelId !== channelId));
  queryClient.removeQueries({ queryKey: keys.messages(channelId) });
  queryClient.removeQueries({ queryKey: keys.members(channelId) });
  queryClient.removeQueries({ queryKey: keys.channelTasks(channelId) });
  queryClient.removeQueries({ queryKey: keys.recordings(channelId) });
}

/** Drops a task that no longer exists (a draft someone discarded) from the workspace list and its channel's list. */
export function removeTask(queryClient: QueryClient, task: { id: string; channelId: string }): void {
  const without = (tasks: TaskView[] | undefined) => tasks?.filter((t) => t.id !== task.id);
  queryClient.setQueryData<TaskView[]>(keys.tasks(), without);
  queryClient.setQueryData<TaskView[]>(keys.channelTasks(task.channelId), without);
}
