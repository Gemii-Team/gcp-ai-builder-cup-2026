import { Button, Icon, IconButton, LangRuns } from '@kiwi/ui';
import { useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { useApi } from '../../api/client';
import { confirmDraft, discardDraft } from '../../api/confirmDraft';
import { useChannelTasks, useUsers } from '../../api/hooks';
import type { TaskView } from '../../api/types';
import { T, useI18n } from '../../i18n';

type DraftTasksPanelProps = {
  meetingId: string;
  channelId: string;
};

/** The draft tasks the AI drew from this meeting, each accepted (confirmed) or rejected (discarded), like Asana's panel. */
export function DraftTasksPanel({ meetingId, channelId }: DraftTasksPanelProps) {
  const { plain } = useI18n();
  const api = useApi();
  const queryClient = useQueryClient();
  const tasks = useChannelTasks(channelId);
  const users = useUsers();
  const [busy, setBusy] = useState<Set<string>>(new Set());

  const userNames = useMemo(() => new Map((users.data ?? []).map((user) => [user.id, user.name])), [users.data]);
  const drafts = useMemo(
    () => (tasks.data ?? []).filter((task) => task.state === 'draft' && task.source?.kind === 'meeting' && task.source.id === meetingId),
    [tasks.data, meetingId],
  );

  function mark(id: string, on: boolean) {
    setBusy((current) => {
      const next = new Set(current);
      if (on) {
        next.add(id);
      } else {
        next.delete(id);
      }
      return next;
    });
  }

  async function accept(task: TaskView) {
    if (task.assigneeId === null) {
      return;
    }
    mark(task.id, true);
    try {
      await confirmDraft(api, queryClient, { id: task.id, channelId });
    } catch {
      // The draft stays; the caches are reconciled by confirmDraft. Leaving it in the list lets the person retry.
    } finally {
      mark(task.id, false);
    }
  }

  async function reject(task: TaskView) {
    mark(task.id, true);
    try {
      await discardDraft(api, queryClient, { id: task.id, channelId });
    } catch {
      // Left in place; the person can try again.
    } finally {
      mark(task.id, false);
    }
  }

  async function acceptAll() {
    await Promise.all(drafts.filter((task) => task.assigneeId !== null).map((task) => accept(task)));
  }

  const assignable = drafts.filter((task) => task.assigneeId !== null).length;

  return (
    <aside className="mtg-drafts" aria-label={plain('draftTasksHeading')}>
      <div className="mtg-drafts-head">
        <h2 className="mtg-drafts-h">
          <T k="draftTasksHeading" />
          <span className="mtg-drafts-count">{drafts.length}</span>
        </h2>
        {assignable > 0 && (
          <Button size="sm" variant="primary" onClick={() => void acceptAll()}>
            <Icon name="task-add" />
            <T k="draftAcceptAll" />
          </Button>
        )}
      </div>
      {drafts.length === 0 ? (
        <p className="mtg-drafts-empty">
          <T k="draftTasksEmpty" />
        </p>
      ) : (
        <ul className="mtg-drafts-list">
          {drafts.map((task) => (
            <li key={task.id} className="mtg-draft" data-busy={busy.has(task.id) || undefined}>
              <span className="mtg-draft-main">
                <span className="mtg-draft-ttl">
                  <LangRuns text={task.title} />
                </span>
                {task.assigneeId !== null && <span className="mtg-draft-owner">{userNames.get(task.assigneeId) ?? ''}</span>}
              </span>
              <span className="mtg-draft-acts">
                <IconButton
                  label={plain('draftReject')}
                  className="mtg-draft-no"
                  disabled={busy.has(task.id)}
                  onClick={() => void reject(task)}
                >
                  <Icon name="close" />
                </IconButton>
                <IconButton
                  label={plain('draftAccept')}
                  className="mtg-draft-yes"
                  disabled={busy.has(task.id) || task.assigneeId === null}
                  onClick={() => void accept(task)}
                >
                  <Icon name="check" />
                </IconButton>
              </span>
            </li>
          ))}
        </ul>
      )}
    </aside>
  );
}
