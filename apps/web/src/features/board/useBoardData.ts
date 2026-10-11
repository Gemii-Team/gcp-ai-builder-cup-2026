import { useMemo } from 'react';
import { useChannels, useTasks } from '../../api/hooks';
import type { TaskView } from '../../api/types';

export const STATUSES = ['todo', 'doing', 'done'] as const;

/** A status's name in `@kiwi/core`'s tables. */
export const STATUS_KEY = { todo: 'statusTodo', doing: 'statusDoing', done: 'statusDone' } as const;

/** What an empty cell of each status says. */
export const EMPTY_KEY = { todo: 'cellEmptyTodo', doing: 'cellEmptyDoing', done: 'cellEmptyDone' } as const;

export type Status = (typeof STATUSES)[number];

/** What the Source type filter offers: the three kinds of source, and `hand` for a task no source stands behind. */
export type SourceType = 'meeting' | 'message' | 'file' | 'hand';

export type BoardFilters = { channelId: string | null; sourceType: SourceType | null };

export const NO_FILTERS: BoardFilters = { channelId: null, sourceType: null };

/** `hand` first, then the sources a task is filed under: the lanes of the Board are drawn in this order of kinds. */
export type LaneKind = 'hand' | 'meeting' | 'file' | 'channel';

export type Lane = {
  /** `hand`, `meeting:<id>`, `file:<id>` or `messages:<channelId>`: unique and stable while the lane is drawn. */
  key: string;
  kind: LaneKind;
  /** The meeting's or file's id; null for the Added by hand lane and a channel's messages. */
  sourceId: string | null;
  /** The lane's channel; null for the Added by hand lane, which holds the tasks of every channel. */
  channelId: string | null;
  /** The source's name (a meeting's title, a file's name, a channel's name), for ordering; null for Added by hand. */
  label: string | null;
  cells: Record<Status, TaskView[]>;
  /** Every task of the lane that passes the filters. */
  total: number;
};

export type BoardData = {
  lanes: Lane[];
  /** The AI's drafts. Only confirmed tasks sit in lanes, and a filter never hides a draft. */
  drafts: TaskView[];
  /** The confirmed tasks that pass the filters, per status. */
  counts: Record<Status, number>;
  /** How many confirmed tasks there are at all, filtered or not: told apart from "no task matches" by an empty board. */
  confirmed: number;
};

const KIND_ORDER: Record<LaneKind, number> = { hand: 0, meeting: 1, file: 2, channel: 3 };

/** The Source type filter value of a task. */
export function sourceTypeOf(task: TaskView): SourceType {
  return task.source === null ? 'hand' : task.source.kind;
}

export function matchesFilters(task: TaskView, filters: BoardFilters): boolean {
  return (filters.channelId === null || task.channelId === filters.channelId) && (filters.sourceType === null || sourceTypeOf(task) === filters.sourceType);
}

function laneOf(task: TaskView): { key: string; kind: LaneKind; sourceId: string | null } {
  const { source } = task;
  if (source === null) {
    return { key: 'hand', kind: 'hand', sourceId: null };
  }
  switch (source.kind) {
    case 'meeting':
      return { key: `meeting:${source.id}`, kind: 'meeting', sourceId: source.id };
    case 'file':
      return { key: `file:${source.id}`, kind: 'file', sourceId: source.id };
    case 'message':
      // A message's lane is its channel's: every task made from any message of the channel files there.
      return { key: `messages:${task.channelId}`, kind: 'channel', sourceId: null };
  }
}

/** Positions are fractional-index strings that sort by byte order, which for their ASCII digits is plain string order. */
function byPosition(a: TaskView, b: TaskView): number {
  if (a.position !== b.position) {
    return a.position < b.position ? -1 : 1;
  }
  // Positions are per channel: two channels' cards can hold the same one in the lane that spans channels.
  return a.channelId === b.channelId ? a.id.localeCompare(b.id) : a.channelId.localeCompare(b.channelId);
}

/** Newest first; a done task with no `done_at` (never sent that way by the server) sinks. */
function byDoneAt(a: TaskView, b: TaskView): number {
  if (a.doneAt === b.doneAt) {
    return byPosition(a, b);
  }
  if (a.doneAt === null || b.doneAt === null) {
    return a.doneAt === null ? 1 : -1;
  }
  return a.doneAt < b.doneAt ? 1 : -1;
}

/**
 * Files the tasks under the source they came from and orders each lane's three cells: To do and Doing by `position`
 * (a move or a confirm puts a card at the top), Done by `done_at`, newest first. `pinned` cards (just confirmed, not yet
 * moved) go first in To do, newest first. Lanes are the Added by hand lane, then meetings, documents and channel
 * messages, each by name; a lane with no task that passes the filters is not drawn.
 */
export function groupBoard(
  tasks: readonly TaskView[],
  filters: BoardFilters,
  channelName: (channelId: string) => string,
  pinned: readonly string[] = [],
): BoardData {
  const confirmed = tasks.filter((task) => task.state === 'confirmed');
  const lanes = new Map<string, Lane>();
  const counts: Record<Status, number> = { todo: 0, doing: 0, done: 0 };
  for (const task of confirmed) {
    if (!matchesFilters(task, filters)) {
      continue;
    }
    const { key, kind, sourceId } = laneOf(task);
    let lane = lanes.get(key);
    if (lane === undefined) {
      const label = kind === 'hand' ? null : kind === 'channel' ? channelName(task.channelId) : (task.source?.label ?? '');
      lane = { key, kind, sourceId, channelId: kind === 'hand' ? null : task.channelId, label, cells: { todo: [], doing: [], done: [] }, total: 0 };
      lanes.set(key, lane);
    }
    lane.cells[task.status].push(task);
    lane.total += 1;
    counts[task.status] += 1;
  }

  const pin = (task: TaskView) => {
    const at = pinned.indexOf(task.id);
    return at === -1 ? Number.POSITIVE_INFINITY : at;
  };
  for (const lane of lanes.values()) {
    lane.cells.todo.sort((a, b) => (pin(a) === pin(b) ? byPosition(a, b) : pin(a) < pin(b) ? -1 : 1));
    lane.cells.doing.sort(byPosition);
    lane.cells.done.sort(byDoneAt);
  }

  const ordered = [...lanes.values()].sort(
    (a, b) => KIND_ORDER[a.kind] - KIND_ORDER[b.kind] || (a.label ?? '').localeCompare(b.label ?? '') || a.key.localeCompare(b.key),
  );
  const drafts = tasks.filter((task) => task.state === 'draft' && task.source !== null).sort((a, b) => a.createdAt.localeCompare(b.createdAt) || a.id.localeCompare(b.id));
  return { lanes: ordered, drafts, counts, confirmed: confirmed.length };
}

const NO_PINS: readonly string[] = [];

/**
 * The Board's data: the one workspace-wide task list (`['tasks']`, which the server bounds to the caller's channels)
 * filed into source lanes. It is a view of the cache, so a move, a confirm or a realtime `task.changed` shows at once.
 */
export function useBoardData(filters: BoardFilters, pinned: readonly string[] = NO_PINS) {
  const tasks = useTasks();
  const channels = useChannels();
  const { channelId, sourceType } = filters;
  const names = useMemo(() => new Map((channels.data ?? []).map((channel) => [channel.id, channel.name])), [channels.data]);
  const board = useMemo(
    () => (tasks.data === undefined ? undefined : groupBoard(tasks.data, { channelId, sourceType }, (id) => names.get(id) ?? '', pinned)),
    [tasks.data, channelId, sourceType, names, pinned],
  );
  return { board, isPending: tasks.isPending, isError: tasks.isError, refetch: tasks.refetch };
}
