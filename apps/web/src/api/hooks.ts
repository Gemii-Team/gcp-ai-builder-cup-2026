import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import { skipToken, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { mapCalendarEvent } from './mappers';
import { useApi } from './client';
import {
  channelsQuery,
  channelTasksQuery,
  fileQuery,
  keys,
  meetingQuery,
  meetingsQuery,
  membersQuery,
  meQuery,
  tasksQuery,
  upcomingEventsQuery,
  usersQuery,
} from './queries';
import type { CalendarEventView, RecordingUpload } from './types';

/** The signed-in user and workspace: the one `GetMe` query. */
export function useMe() {
  return useQuery(meQuery(useApi()));
}

export function useUsers() {
  return useQuery(usersQuery(useApi()));
}

export function useChannels() {
  return useQuery(channelsQuery(useApi()));
}

export function useMembers(channelId: string) {
  return useQuery(membersQuery(useApi(), channelId));
}

/** Every task of the channels the person belongs to: the one workspace-wide `ListTasks` the Board reads. */
export function useTasks() {
  return useQuery(tasksQuery(useApi()));
}

export function useChannelTasks(channelId: string) {
  return useQuery(channelTasksQuery(useApi(), channelId));
}

export function useFile(id: string) {
  return useQuery(fileQuery(useApi(), id));
}

export function useMeeting(id: string) {
  return useQuery(meetingQuery(useApi(), id));
}

/** Every meeting of the channels the person belongs to, newest first. */
export function useMeetings() {
  return useQuery(meetingsQuery(useApi()));
}

/** Re-runs a failed meeting's summary, then refreshes the meeting and the list. */
export function useRetrySummary() {
  const api = useApi();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (meetingId: string) => {
      await api.meetings.retrySummary({ meetingId });
    },
    onSuccess: (_result, meetingId) => {
      void queryClient.invalidateQueries({ queryKey: keys.meeting(meetingId) });
      void queryClient.invalidateQueries({ queryKey: keys.meetings() });
    },
  });
}

/** The recordings this session uploaded into a channel (none after a reload: only the uploading session follows its recording). */
export function useRecordings(channelId: string): RecordingUpload[] {
  return useQuery<RecordingUpload[]>({ queryKey: keys.recordings(channelId), queryFn: skipToken, staleTime: Infinity }).data ?? [];
}

/** Every upcoming event the person may see: their teams' events and the workspace's org-wide ones. */
export function useUpcomingEvents() {
  return useQuery(upcomingEventsQuery(useApi()));
}

/** What a channel (team) is scheduled into, and whether the whole workspace should see the meeting. */
export type ScheduleMeetingInput = {
  channelId: string;
  title: string;
  startAt: Date;
  durationMinutes: number;
  orgWide: boolean;
};

/** Schedules a meeting and refreshes the upcoming list so the new event appears in its group. */
export function useScheduleMeeting() {
  const api = useApi();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: ScheduleMeetingInput): Promise<CalendarEventView> => {
      const { event } = await api.calendar.scheduleMeeting({
        channelId: input.channelId,
        title: input.title,
        startAt: timestampFromDate(input.startAt),
        durationMinutes: input.durationMinutes,
        orgWide: input.orgWide,
      });
      if (event === undefined) {
        throw new Error('ScheduleMeeting answered without an event.');
      }
      return mapCalendarEvent(event);
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: keys.events() }),
  });
}

/** Cancels a scheduled event and refreshes the upcoming list so it drops out. */
export function useCancelEvent() {
  const api = useApi();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (eventId: string) => {
      await api.calendar.cancelEvent({ eventId });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: keys.events() }),
  });
}
