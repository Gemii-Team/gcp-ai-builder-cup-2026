import '../shell/screen.css';
import './channel.css';
import { DraftsBar, DraftsPanel, ExcerptProvider, formatClock, Pending } from '@kiwi/ui';
import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useId, useMemo, useRef, type ReactNode } from 'react';
import { useApi } from '../../api/client';
import { createExcerptLoader } from '../../api/excerpts';
import { useChannels, useChannelTasks, useMe, useMembers, useUsers } from '../../api/hooks';
import type { ChannelView, ChatMessage } from '../../api/types';
import { T, useI18n } from '../../i18n';
import { useUiStore } from '../../state/ui';
import { AppLink } from '../shell/AppLink';
import { AttachButton, UploadNotice } from './Attach';
import { ChannelHeader } from './ChannelHeader';
import { Composer, type MentionTarget } from './Composer';
import { MadeTasks, MakeTask } from './MakeTask';
import { MessageAction } from './MessageAction';
import { MessageFiles } from './MessageFiles';
import { MessageRow } from './MessageRow';
import { Stream } from './Stream';
import { useChannelMessages } from './useChannelMessages';
import { DraftsSheet } from './DraftsSheet';
import { useChannelDrafts } from './useChannelDrafts';
import { useMakeTask } from './useMakeTask';
import { useStartMeeting } from './useStartMeeting';
import { useUpload } from './useUpload';

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
  const { lang, plain } = useI18n();
  const markRead = useUiStore((state) => state.markRead);
  const setLastChannel = useUiStore((state) => state.setLastChannel);
  const me = useMe();
  const users = useUsers();
  const members = useMembers(channel.id);
  const tasks = useChannelTasks(channel.id);
  const stream = useChannelMessages(channel.id);
  const upload = useUpload(channel.id);
  const make = useMakeTask();
  const meeting = useStartMeeting(channel.id);
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

  const drafts = useChannelDrafts(channel, (id) => names.get(id));
  const panel = { drafts: drafts.drafts, sealed: drafts.sealed, members: drafts.members, onConfirm: drafts.onConfirm, onEdit: drafts.onEdit, counts: drafts.counts, variant: 'panel' } as const;
  const sheetOpen = useUiStore((state) => state.draftsSheetOpen);
  const setSheetOpen = useUiStore((state) => state.setDraftsSheetOpen);
  const sheetId = useId();
  const bar = useRef<HTMLButtonElement>(null);

  /** The drafts a meeting summary made that are still waiting; unknown until the channel's tasks have loaded. */
  const draftsOf = (message: ChatMessage): number | undefined =>
    message.artifact?.kind !== 'meeting_summary' || tasks.data === undefined
      ? undefined
      : tasks.data.filter((task) => task.state === 'draft' && task.source?.kind === 'meeting' && task.source.id === message.artifact?.meetingId).length;

  return (
    <ExcerptProvider load={load}>
      <div className="chan">
        <main className="main">
          <ChannelHeader channel={channel} memberCount={members.data?.length} onStartMeeting={() => void meeting.start()} starting={meeting.starting} />
          {meeting.failed && (
            <p className="notice head-notice" role="alert">
              <T k="startMeetingFailed" />
            </p>
          )}
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
            renderRow={(message) => {
              const authorName = names.get(message.authorId) ?? '';
              const human = message.authorKind === 'user' && message.delivery === undefined;
              const formId = `make-task-${message.id}`;
              return (
                <MessageRow
                  message={message}
                  authorName={authorName}
                  draftCount={draftsOf(message)}
                  action={
                    human ? (
                      <MessageAction
                        authorName={authorName}
                        time={formatClock(message.createdAt)}
                        expanded={make.openId === message.id}
                        controls={formId}
                        onPress={() => {
                          make.toggle(message.id);
                        }}
                      />
                    ) : undefined
                  }
                  below={
                    <>
                      <MessageFiles message={message} />
                      {human && make.openId === message.id && (
                        <MakeTask
                          id={formId}
                          message={message}
                          channel={channel}
                          authorName={authorName}
                          onCreated={(task) => {
                            make.made(task.id, plain('taskMadeAnnouncement', { code: task.ref, title: task.title }));
                          }}
                          onCancel={() => {
                            make.cancel(message.id);
                          }}
                        />
                      )}
                      {human && (
                        <MadeTasks message={message} channel={channel} authorName={authorName} ownerNameOf={(id) => names.get(id)} focusTaskId={make.focusTaskId} />
                      )}
                    </>
                  }
                  onRetry={() => {
                    void stream.retry(message.clientMsgId);
                  }}
                />
              );
            }}
          />
          <DraftsBar
            ref={bar}
            count={drafts.drafts.length}
            expanded={sheetOpen}
            controls={sheetId}
            onPress={() => {
              setSheetOpen(true);
            }}
          />
          <Composer
            channelName={channel.name}
            people={people}
            canSend={stream.messages !== undefined}
            onSend={stream.send}
            attach={<AttachButton onPick={(file) => void upload.upload(file)} />}
            notice={<UploadNotice state={upload.state} onDismiss={upload.dismiss} />}
          />
          <p role="status" className="sr-only">
            {make.announcement}
          </p>
        </main>
        <DraftsPanel {...panel} />
      </div>
      <DraftsSheet id={sheetId} open={sheetOpen} onOpenChange={setSheetOpen} opener={bar}>
        <DraftsPanel {...panel} />
      </DraftsSheet>
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
