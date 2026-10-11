import './channel.css';
import { ExcerptProvider, Pending } from '@kiwi/ui';
import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useMemo, type ReactNode } from 'react';
import { useApi } from '../../api/client';
import { createExcerptLoader } from '../../api/excerpts';
import { useChannels, useChannelTasks, useMe, useMembers, useUsers } from '../../api/hooks';
import type { ChannelView, ChatMessage } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { useUiStore } from '../../state/ui';
import { AppLink } from '../shell/AppLink';
import { ChannelHeader } from './ChannelHeader';
import { Composer, type MentionTarget } from './Composer';
import { MessageRow } from './MessageRow';
import { Stream } from './Stream';
import { useChannelMessages } from './useChannelMessages';

/** The agent's mention: the one agent there is, called by its constant label. */
const ASSISTANT: MentionTarget = { kind: 'agent', id: 'assistant', name: 'assistant' };

function Notice({ children }: { children: ReactNode }) {
  return (
    <div className="chan">
      <main className="main">
        <div className="stream-in notice-page">{children}</div>
      </main>
    </div>
  );
}

function ChannelContent({ channel }: { channel: ChannelView }) {
  const api = useApi();
  const queryClient = useQueryClient();
  const { lang } = useI18n();
  const markRead = useUiStore((state) => state.markRead);
  const setLastChannel = useUiStore((state) => state.setLastChannel);
  const me = useMe();
  const users = useUsers();
  const members = useMembers(channel.id);
  const tasks = useChannelTasks(channel.id);
  const stream = useChannelMessages(channel.id);
  const load = useMemo(() => createExcerptLoader(api, queryClient, lang), [api, queryClient, lang]);

  // The stream shows every message the channel has, so the channel is read up to its head while it is open.
  useEffect(() => {
    markRead(channel.id, channel.lastSeq);
    setLastChannel(channel.id);
  }, [channel.id, channel.lastSeq, markRead, setLastChannel]);

  const names = useMemo(() => {
    const byId = new Map(members.data?.map((member) => [member.userId, member.name]));
    for (const user of users.data ?? []) {
      byId.set(user.id, user.name);
    }
    return byId;
  }, [members.data, users.data]);

  const people = useMemo(
    () => [ASSISTANT, ...(members.data ?? []).filter((member) => member.userId !== me.data?.user.id).map((member): MentionTarget => ({ kind: 'user', id: member.userId, name: member.name }))],
    [members.data, me.data],
  );

  /** The drafts a meeting summary made that are still waiting; unknown until the channel's tasks have loaded. */
  const draftsOf = (message: ChatMessage): number | undefined =>
    message.artifact?.kind !== 'meeting_summary' || tasks.data === undefined
      ? undefined
      : tasks.data.filter((task) => task.state === 'draft' && task.source?.kind === 'meeting' && task.source.id === message.artifact?.meetingId).length;

  return (
    <ExcerptProvider load={load}>
      <div className="chan">
        <main className="main">
          <ChannelHeader channel={channel} memberCount={members.data?.length} />
          <Stream
            channel={channel}
            messages={stream.messages}
            failed={stream.isError}
            onReload={() => {
              void stream.refetch();
            }}
            hasEarlier={stream.hasEarlier}
            loadingEarlier={stream.loadingEarlier}
            earlierFailed={stream.earlierFailed}
            onLoadEarlier={() => {
              void stream.loadEarlier();
            }}
            renderRow={(message) => (
              <MessageRow
                message={message}
                authorName={names.get(message.authorId) ?? ''}
                draftCount={draftsOf(message)}
                onRetry={() => {
                  void stream.retry(message.clientMsgId);
                }}
              />
            )}
          />
          <Composer channelName={channel.name} people={people} canSend={stream.messages !== undefined} onSend={stream.send} />
        </main>
      </div>
    </ExcerptProvider>
  );
}

/**
 * What the route `/c/$channelId` shows: the channel's header, its stream and its composer. A channel the person is not in
 * (or was removed from while it was open) is said to be unavailable, never shown from the cache.
 */
export function ChannelScreen({ channelId }: { channelId: string }) {
  const { plain } = useI18n();
  const channels = useChannels();
  const channel = channels.data?.find((candidate) => candidate.id === channelId);

  if (channels.isPending) {
    return (
      <Notice>
        <Pending label={plain('loading')} />
      </Notice>
    );
  }
  if (channel === undefined) {
    return (
      <Notice>
        <p className="notice" role="alert">
          <T k="channelUnavailable" />{' '}
          <AppLink to="/" className="lk">
            <T k="goToChannels" />
          </AppLink>
        </p>
      </Notice>
    );
  }
  // One channel at a time: switching channels starts its state afresh instead of carrying a draft message or an open form over.
  return <ChannelContent key={channel.id} channel={channel} />;
}
