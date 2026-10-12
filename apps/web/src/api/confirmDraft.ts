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
    // A list of the tasks that was asked for before the confirmation was answered from before it: if it lands after the
    // write it would put the draft back (and nothing repairs it until the next load). It is dropped first; `cancelQueries`
    // must be awaited, since it restores the query's state from before that fetch when it settles.
    await queryClient.cancelQueries({ queryKey: keys.tasks() });
    writeTask(queryClient, view);
    return view;
  } catch (error) {
    dropIfGone(queryClient, draft, error);
    throw error;
  }
}

/**
 * A draft the server no longer has as a draft: someone discarded it (NotFound) or confirmed it first (Aborted, the conflict
 * the service answers with for a task that is not a draft). Its task leaves the caches and the lists are loaded again, so
 * what the screens show is what is there. Any other error changes nothing.
 */
function dropIfGone(queryClient: QueryClient, draft: { id: string; channelId: string }, error: unknown): boolean {
  const gone = hasCode(error, Code.NotFound) || hasCode(error, Code.Aborted);
  if (gone) {
    removeTask(queryClient, draft);
    void queryClient.invalidateQueries({ queryKey: keys.tasks() });
  }
  return gone;
}

/**
 * Discards a draft on the server and takes its task out of the task caches. A draft that is already gone, or that someone
 * confirmed first, is what the person wanted or better: it too leaves the caches (and the lists are loaded again) and
 * nothing is thrown. Any other failure leaves the caches as they were and rejects.
 */
export async function discardDraft(api: Api, queryClient: QueryClient, draft: { id: string; channelId: string }): Promise<void> {
  try {
    await api.tasks.discardDraft({ id: draft.id });
    removeTask(queryClient, draft);
  } catch (error) {
    if (!dropIfGone(queryClient, draft, error)) {
      throw error;
    }
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
