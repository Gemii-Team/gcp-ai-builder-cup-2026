import { Code } from '@connectrpc/connect';
import { TaskStatus } from '@kiwi/api-client';
import { useQueryClient, type QueryClient } from '@tanstack/react-query';
import { useCallback } from 'react';
import { useApi, type Api } from '../../api/client';
import { hasCode } from '../../api/errors';
import { mapTask } from '../../api/mappers';
import { keys, tasksQuery, writeTask } from '../../api/queries';
import type { TaskView } from '../../api/types';
import type { Status } from './useBoardData';

const STATUS: Record<Status, TaskStatus> = { todo: TaskStatus.TODO, doing: TaskStatus.DOING, done: TaskStatus.DONE };

/** The card a moved task must sit above to be at the top: the first confirmed card the destination status holds in the task's channel. */
function topCard(tasks: readonly TaskView[], task: TaskView, to: Status): string | undefined {
  let top: TaskView | undefined;
  for (const other of tasks) {
    if (other.id !== task.id && other.channelId === task.channelId && other.status === to && other.state === 'confirmed' && (top === undefined || other.position < top.position)) {
      top = other;
    }
  }
  return top?.id;
}

/**
 * What the Board shows while the server thinks: the card already in the destination status, first. An empty position
 * sorts before every real one, a done card is the newest done one, and the server's answer then replaces both.
 */
function moved(task: TaskView, to: Status): TaskView {
  return { ...task, status: to, position: '', doneAt: to === 'done' ? new Date().toISOString() : null };
}

async function send(api: Api, queryClient: QueryClient, task: TaskView, to: Status): Promise<TaskView> {
  const before = topCard(queryClient.getQueryData<TaskView[]>(keys.tasks()) ?? [], task, to);
  const { task: answer } = await api.tasks.moveTask({ id: task.id, status: STATUS[to], ...(before === undefined ? {} : { beforeId: before }) });
  if (answer === undefined) {
    throw new Error('MoveTask answered without a task.');
  }
  return mapTask(answer);
}

/**
 * Moves one card to another status of its own lane, to the top of that status (`MoveTask` with `before_id` the first
 * card there, no `after_id`; both empty for an empty status). Optimistic: the card moves in the cache at once and goes
 * back where it was if the server refuses. The server answers Aborted when the lane changed underneath the request: the
 * tasks are loaded again and the move is tried once more against what is there now.
 */
export function useMoveTask(taskId: string): (to: Status) => Promise<void> {
  const api = useApi();
  const queryClient = useQueryClient();
  return useCallback(
    async (to) => {
      let current = queryClient.getQueryData<TaskView[]>(keys.tasks())?.find((task) => task.id === taskId);
      for (let attempt = 0; current !== undefined && current.status !== to; attempt += 1) {
        const before = current;
        const optimistic = moved(before, to);
        writeTask(queryClient, optimistic);
        try {
          writeTask(queryClient, await send(api, queryClient, before, to));
          return;
        } catch (error) {
          // Back to where it was, unless something newer (a realtime event) has already replaced the optimistic card.
          // The cache shares structure, so the card is compared by what it holds, not by identity.
          const now = queryClient.getQueryData<TaskView[]>(keys.tasks())?.find((task) => task.id === taskId);
          if (now?.status === optimistic.status && now.position === optimistic.position && now.doneAt === optimistic.doneAt) {
            writeTask(queryClient, before);
          }
          if (attempt > 0 || !hasCode(error, Code.Aborted)) {
            throw error;
          }
          current = (await queryClient.fetchQuery({ ...tasksQuery(api), staleTime: 0 })).find((task) => task.id === taskId);
        }
      }
    },
    [api, queryClient, taskId],
  );
}
