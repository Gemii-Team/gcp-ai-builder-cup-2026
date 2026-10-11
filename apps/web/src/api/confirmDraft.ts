import { Code } from '@connectrpc/connect';
import type { DraftPatch } from '@kiwi/ui';
import type { QueryClient } from '@tanstack/react-query';
import type { Api } from './client';
import { hasCode } from './errors';
import { mapTask } from './mappers';
import { keys, removeTask, writeTask } from './queries';
import type { TaskView } from './types';

/**
 * Confirms a draft on the server and writes the confirmed task into the task caches. A draft that no longer exists
 * (someone discarded it: `DiscardDraft` deletes the row and sends no event) or that someone else confirmed first is
 * dropped from the caches and the task lists are loaded again, and the confirmation still rejects, so the caller seals
 * nothing for it. Any other failure leaves the caches as they were.
 */
export async function confirmDraft(api: Api, queryClient: QueryClient, draft: { id: string; channelId: string }): Promise<TaskView> {
  try {
    const { task } = await api.tasks.confirmDraft({ id: draft.id });
    if (task === undefined) {
      throw new Error('ConfirmDraft answered without a task.');
    }
    const view = mapTask(task);
    writeTask(queryClient, view);
    return view;
  } catch (error) {
    if (hasCode(error, Code.NotFound) || hasCode(error, Code.Aborted)) {
      removeTask(queryClient, draft);
      void queryClient.invalidateQueries({ queryKey: keys.tasks() });
    }
    throw error;
  }
}

/** Saves a change to a draft (its title, or its owner: `null` clears it) and writes the updated task into the caches. */
export async function editDraft(api: Api, queryClient: QueryClient, id: string, patch: DraftPatch): Promise<void> {
  const { task } = await api.tasks.updateTask({
    id,
    ...(patch.title === undefined ? {} : { title: patch.title }),
    ...(patch.ownerId === undefined ? {} : patch.ownerId === null ? { clearAssignee: true } : { assigneeId: patch.ownerId }),
  });
  if (task !== undefined) {
    writeTask(queryClient, mapTask(task));
  }
}
