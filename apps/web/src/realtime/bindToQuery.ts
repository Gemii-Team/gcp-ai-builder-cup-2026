import type { QueryClient } from '@tanstack/react-query';
import { MembershipChange, type FileStatusChanged, type MeetingSummaryReady, type MembershipChanged } from '@kiwi/api-client';
import type { RealtimeClient } from '@kiwi/core';
import { mapFileStatus, mapMeetingStatus, mapMessage, mapTask } from '../api/mappers';
import { keys, receiveMessage, removeChannel, writeTask } from '../api/queries';
import type { FileView, MeetingView, SessionView } from '../api/types';

function fileStatus(queryClient: QueryClient, event: FileStatusChanged, now: Date) {
  const status = mapFileStatus(event.status);
  queryClient.setQueryData<FileView>(keys.file(event.fileId), (file) =>
    file === undefined ? file : { ...file, status, failureReason: event.failureReason, settledAt: now.toISOString() },
  );
  if (status === 'ready') {
    // A ready file has its REF and page count now, which only the server can say.
    void queryClient.invalidateQueries({ queryKey: keys.file(event.fileId) });
  }
}

function meetingStatus(queryClient: QueryClient, event: MeetingSummaryReady, now: Date) {
  const status = mapMeetingStatus(event.status);
  queryClient.setQueryData<MeetingView>(keys.meeting(event.meetingId), (meeting) =>
    meeting === undefined ? meeting : { ...meeting, status, failureReason: event.failureReason, settledAt: now.toISOString() },
  );
  if (status === 'ready') {
    void queryClient.invalidateQueries({ queryKey: keys.meeting(event.meetingId) });
  }
}

function membership(queryClient: QueryClient, event: MembershipChanged) {
  const me = queryClient.getQueryData<SessionView>(keys.me());
  const mine = me === undefined || me.user.id === event.userId;
  if (!mine) {
    void queryClient.invalidateQueries({ queryKey: keys.members(event.channelId) });
  } else if (event.change === MembershipChange.REMOVED) {
    removeChannel(queryClient, event.channelId);
  } else {
    void queryClient.invalidateQueries({ queryKey: keys.channels() });
  }
}

/**
 * Applies what the gateway sends straight to the query cache, so the cache is the one source of truth for the screens:
 * a message joins its channel once however it arrives, a changed task replaces itself in the workspace and channel
 * lists, a file or a meeting changes status, a membership change adds or drops a channel, and a channel that is too far
 * behind to replay (`reset`) is loaded again from its latest page. Returns the function that stops it.
 */
export function bindToQuery(client: RealtimeClient, queryClient: QueryClient, now: () => Date = () => new Date()): () => void {
  return client.on((event) => {
    if ('type' in event) {
      void queryClient.invalidateQueries({ queryKey: keys.messages(event.channelId) });
      return;
    }
    switch (event.event.case) {
      case 'messageCreated':
        receiveMessage(queryClient, mapMessage(event.event.value));
        break;
      case 'taskChanged':
        writeTask(queryClient, mapTask(event.event.value));
        break;
      case 'fileStatusChanged':
        fileStatus(queryClient, event.event.value, now());
        break;
      case 'meetingSummaryReady':
        meetingStatus(queryClient, event.event.value, now());
        break;
      case 'membershipChanged':
        membership(queryClient, event.event.value);
        break;
      default:
        // Progress, presence and typing frames belong to the screens that show them.
        break;
    }
  });
}
