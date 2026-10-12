// The Board's test world: a workspace with two channels, a meeting, a document, a channel's messages and a hand-made
// task, on Tue 13 Oct 2026 at 14:40 in Bangkok, and the queries that read the screen. Test support: nothing in the app
// imports this file.
import { create } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import { CreatedByKind, GetFileResponseSchema, GetMeetingResponseSchema, SourceKind, TaskStatus } from '@kiwi/api-client';
import { dateToDueAt } from '@kiwi/ui';
import { screen, within } from '@testing-library/react';
import { vi } from 'vitest';
import { BoardScreen } from '../features/board/Board';
import { useUiStore } from '../state/ui';
import { at, channel, file, meeting, ME_ID, task } from './fixtures';
import { renderApp } from './render';
import { createStubApi, seedApi, type StubApi, type World } from './stubApi';

export const CHANNELS = [channel({ id: 'ch-q4', name: 'q4-launch' }), channel({ id: 'ch-ops', name: 'ops-warehouse' })];

export const MEETING = { kind: SourceKind.MEETING, id: 'meeting-1', anchor: '12', label: 'Q4 launch weekly sync', channelId: 'ch-q4', offsetMs: 761000n };
export const QUOTE = '4.1 Term. This Agreement ends on 31 December 2026.';
export const DOCUMENT = { kind: SourceKind.FILE, id: 'file-1', anchor: 'page:4', label: 'ACME-contract-v3.pdf', channelId: 'ch-q4', page: 4, quote: QUOTE };
export const MESSAGE = { kind: SourceKind.MESSAGE, id: 'msg-41', anchor: '41', label: 'Arthit K.', channelId: 'ch-ops', at: timestampFromDate(at('17:05', '2026-10-12')) };

/** A due date as the server stores it: midnight in Asia/Bangkok. */
export const due = (day: string) => timestampFromDate(dateToDueAt(day));

export const ANNOUNCE = task({ id: 't-announce', ref: 'TSK-2030', title: 'Announce the new launch date', assigneeId: ME_ID, dueAt: due('2026-10-14'), position: 'a0', source: MEETING });
export const RATE_CARD = task({ id: 't-ratecard', ref: 'TSK-2024', title: 'Get the Q4 rate card', assigneeId: 'user-nok', dueAt: due('2026-10-12'), position: 'a1', source: MEETING });
export const RENEW = task({ id: 't-renew', ref: 'TSK-2026', title: 'Decide whether to renew ACME', position: 'a0', source: DOCUMENT });
export const FORKLIFT = task({ id: 't-forklift', ref: 'TSK-2028', channelId: 'ch-ops', title: 'Book two extra forklift shifts', assigneeId: 'user-arthit', position: 'a0', source: MESSAGE });
export const BRIEFING = task({
  id: 't-hand',
  ref: 'TSK-2036',
  channelId: 'ch-ops',
  title: 'Prepare the warehouse briefing',
  assigneeId: 'user-arthit',
  dueAt: due('2026-10-20'),
  position: 'a0',
  createdById: ME_ID,
  createdByKind: CreatedByKind.USER,
  createdTime: '13:20',
});
export const SIGNED = task({ id: 't-signed', ref: 'TSK-2017', title: 'Send the signed contract to Finance', status: TaskStatus.DONE, assigneeId: ME_ID, doneAt: timestampFromDate(at('14:48')), position: 'a0', source: DOCUMENT });
export const TASKS = [ANNOUNCE, RATE_CARD, RENEW, FORKLIFT, BRIEFING, SIGNED];

/** "Now" is 14:40 on Tue 13 Oct 2026 in Bangkok; the interface starts in English with nothing read and the sheet shut. */
export function startBoardDay() {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(at('14:40'));
}

export function endBoardDay() {
  vi.useRealTimers();
  vi.restoreAllMocks();
}

/** The Board in the signed-in shell, on a stubbed API that answers GetMeeting and GetFile for the lanes' meta. */
export function openBoard(world: World = {}, api: StubApi = createStubApi(), initialChannelId: string | null = null) {
  seedApi(api, { channels: CHANNELS, tasks: TASKS, ...world });
  api.meetings.getMeeting.mockResolvedValue(create(GetMeetingResponseSchema, { meeting: meeting({ id: 'meeting-1', ref: 'MTG-1014', startedAt: timestampFromDate(at('13:46')), endedAt: timestampFromDate(at('14:29')) }) }));
  api.files.getFile.mockResolvedValue(create(GetFileResponseSchema, { file: file({ id: 'file-1', name: 'ACME-contract-v3.pdf', ref: 'DOC-0371', pages: 18 }) }));
  return renderApp({ path: '/board', api, routes: [{ path: '/board', element: <BoardScreen initialChannelId={initialChannelId} /> }] });
}

/** The card of a task, by its title. */
export const card = (title: string) => {
  const item = screen.getByRole('heading', { level: 3, name: title }).closest('li');
  if (item === null) {
    throw new Error(`"${title}" is not on a card`);
  }
  return within(item);
};

export const lane = (name: RegExp | string) => within(screen.getByRole('region', { name }));
export const cell = (status: string, laneTitle: string) => within(screen.getByRole('group', { name: `${status} in ${laneTitle}` }));

export async function ready() {
  await screen.findByRole('heading', { level: 3, name: 'Announce the new launch date' });
}
