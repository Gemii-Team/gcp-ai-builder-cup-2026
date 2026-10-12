import { skipToken, useQuery } from '@tanstack/react-query';
import { useApi } from './client';
import { channelsQuery, channelTasksQuery, fileQuery, keys, meetingQuery, membersQuery, meQuery, tasksQuery, usersQuery } from './queries';
import type { RecordingUpload } from './types';

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

/** The recordings this session uploaded into a channel (none after a reload: only the uploading session follows its recording). */
export function useRecordings(channelId: string): RecordingUpload[] {
  return useQuery<RecordingUpload[]>({ queryKey: keys.recordings(channelId), queryFn: skipToken, staleTime: Infinity }).data ?? [];
}
