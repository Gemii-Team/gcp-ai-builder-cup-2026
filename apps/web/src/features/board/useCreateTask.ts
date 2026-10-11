import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import { dateToDueAt, type TaskFormValues } from '@kiwi/ui';
import { useQueryClient } from '@tanstack/react-query';
import { useCallback } from 'react';
import { useApi } from '../../api/client';
import { mapTask } from '../../api/mappers';
import { writeTask } from '../../api/queries';
import type { TaskView } from '../../api/types';

/**
 * Makes a task by hand: `CreateTask` with a title, a channel, an owner and a due date (midnight Asia/Bangkok), and no
 * `source` and no `state`, so the server confirms it at once and it never is a draft. The task the server returns goes
 * into the task caches before its realtime event arrives.
 */
export function useCreateTask(): (values: TaskFormValues) => Promise<TaskView> {
  const api = useApi();
  const queryClient = useQueryClient();
  return useCallback(
    async ({ title, channelId, ownerId, dueDate }) => {
      const { task } = await api.tasks.createTask({
        channelId,
        title,
        ...(ownerId === null ? {} : { assigneeId: ownerId }),
        ...(dueDate === null ? {} : { dueAt: timestampFromDate(dateToDueAt(dueDate)) }),
      });
      if (task === undefined) {
        throw new Error('CreateTask answered without a task.');
      }
      const view = mapTask(task);
      writeTask(queryClient, view);
      return view;
    },
    [api, queryClient],
  );
}
