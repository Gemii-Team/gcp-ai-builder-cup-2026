import { create } from '@bufbuild/protobuf';
import { ConfirmDraftResponseSchema, ListTasksResponseSchema, TaskState, type ListTasksResponse } from '@kiwi/api-client';
import { describe, expect, it } from 'vitest';
import { task } from '../testing/fixtures';
import { createStubApi } from '../testing/stubApi';
import { confirmDraft } from './confirmDraft';
import { createQueryClient } from './queryClient';
import { keys, tasksQuery } from './queries';
import type { TaskView } from './types';

describe('confirmDraft', () => {
  it('keeps the confirmed task when a ListTasks that was already in flight answers after the confirmation', async () => {
    const api = createStubApi();
    const queryClient = createQueryClient();
    const draft = task({ id: 'task-9', state: TaskState.DRAFT, assigneeId: 'user-pim' });
    const confirmed = task({ id: 'task-9', state: TaskState.CONFIRMED, assigneeId: 'user-pim' });
    queryClient.setQueryData(keys.tasks(), [{ id: 'task-9', channelId: 'ch-q4', state: 'draft' }] as TaskView[]);

    // A list of the workspace's tasks was asked for before the confirmation and the server answered it from before it: the
    // answer is still on its way when the confirmation is written to the cache.
    const listed = Promise.withResolvers<ListTasksResponse>();
    api.tasks.listTasks.mockReturnValue(listed.promise);
    const listing = queryClient.fetchQuery({ ...tasksQuery(api), staleTime: 0 }).catch(() => undefined);
    api.tasks.confirmDraft.mockResolvedValue(create(ConfirmDraftResponseSchema, { task: confirmed }));

    await confirmDraft(api, queryClient, { id: 'task-9', channelId: 'ch-q4' });
    listed.resolve(create(ListTasksResponseSchema, { tasks: [draft] }));
    await listing;

    expect(queryClient.getQueryData<TaskView[]>(keys.tasks())?.map((t) => t.state)).toEqual(['confirmed']);
  });
});
