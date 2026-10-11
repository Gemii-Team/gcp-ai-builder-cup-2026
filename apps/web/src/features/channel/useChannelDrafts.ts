import { Code } from '@connectrpc/connect';
import { formatDay, type BoardCounts, type DraftPatch, type DraftTask, type Member, type SealedTask } from '@kiwi/ui';
import { useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useApi } from '../../api/client';
import { hasCode } from '../../api/errors';
import { useChannelTasks, useMe, useMembers } from '../../api/hooks';
import { mapTask } from '../../api/mappers';
import { keys, removeTask, writeTask } from '../../api/queries';
import type { ChannelView, TaskView } from '../../api/types';
import { useI18n } from '../../i18n';

/**
 * The drafts panel's data: the channel's AI drafts (tasks in the draft state that have a source), the ones this person
 * confirmed a moment ago, who they can be given to, and the Board counts of the channel. A draft leaves the list in the
 * same render that its sealed slip enters it, which is what the tear-and-seal waits for. A draft that no longer exists
 * (discarded by someone, or confirmed by someone else first) is dropped here and the tasks are loaded again, and the
 * confirmation still fails, so nothing is sealed for it.
 */
export function useChannelDrafts(channel: ChannelView, ownerNameOf: (userId: string) => string | undefined) {
  const api = useApi();
  const queryClient = useQueryClient();
  const { lang } = useI18n();
  const tasks = useChannelTasks(channel.id).data;
  const members = useMembers(channel.id).data;
  const me = useMe().data;
  const [sealed, setSealed] = useState<SealedTask[]>([]);

  const confirmed = new Set(sealed.map((slip) => slip.id));
  const dueLabel = (task: TaskView) => (task.dueAt === null ? {} : { dueLabel: formatDay(task.dueAt, lang) });
  const drafts: DraftTask[] = (tasks ?? []).flatMap((task) =>
    task.state === 'draft' && task.source !== null && !confirmed.has(task.id)
      ? [{ id: task.id, ref: task.ref, title: task.title, ownerId: task.assigneeId, source: task.source, ...dueLabel(task), ...(task.fromRef === null ? {} : { fromAnswerRef: task.fromRef }) }]
      : [],
  );
  const memberList: Member[] = (members ?? []).map((member) => ({ id: member.userId, name: member.name }));
  const counts: BoardCounts = { todo: 0, doing: 0, done: 0 };
  for (const task of tasks ?? []) {
    if (task.state === 'confirmed') {
      counts[task.status] += 1;
    }
  }

  async function onConfirm(id: string) {
    try {
      const { task } = await api.tasks.confirmDraft({ id });
      if (task === undefined) {
        throw new Error('ConfirmDraft answered without a task.');
      }
      const view = mapTask(task);
      writeTask(queryClient, view);
      const { source } = view;
      if (source !== null) {
        const ownerName = view.assigneeId === null ? undefined : ownerNameOf(view.assigneeId);
        setSealed((slips) => [
          {
            id: view.id,
            ref: view.ref,
            title: view.title,
            ...(ownerName === undefined ? {} : { ownerName }),
            ...dueLabel(view),
            source,
            confirmedBy: me?.user.name ?? '',
            confirmedAt: new Date().toISOString(),
          },
          ...slips,
        ]);
      }
    } catch (error) {
      if (hasCode(error, Code.NotFound) || hasCode(error, Code.Aborted)) {
        removeTask(queryClient, { id, channelId: channel.id });
        void queryClient.invalidateQueries({ queryKey: keys.tasks() });
      }
      throw error;
    }
  }

  async function onEdit(id: string, patch: DraftPatch) {
    const { task } = await api.tasks.updateTask({
      id,
      ...(patch.title === undefined ? {} : { title: patch.title }),
      ...(patch.ownerId === undefined ? {} : patch.ownerId === null ? { clearAssignee: true } : { assigneeId: patch.ownerId }),
    });
    if (task !== undefined) {
      writeTask(queryClient, mapTask(task));
    }
  }

  return { drafts, sealed, members: memberList, counts, onConfirm, onEdit };
}
