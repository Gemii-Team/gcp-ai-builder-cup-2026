import { create } from '@bufbuild/protobuf';
import { CreateTaskResponseSchema } from '@kiwi/api-client';
import { renderHook } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { mapTask } from '../../api/mappers';
import { keys } from '../../api/queries';
import type { TaskView } from '../../api/types';
import { task } from '../../testing/fixtures';
import { createTestQueryClient, hookWrapper } from '../../testing/render';
import { createStubApi } from '../../testing/stubApi';
import { useCreateTask } from './useCreateTask';

describe('useCreateTask', () => {
  it('asks for a confirmed task by hand: no source, no state, the due date as midnight in Bangkok', async () => {
    const api = createStubApi();
    const queryClient = createTestQueryClient();
    queryClient.setQueryData(keys.tasks(), [mapTask(task({ id: 'existing' }))]);
    queryClient.setQueryData(keys.channelTasks('ch-q4'), [mapTask(task({ id: 'existing' }))]);
    api.tasks.createTask.mockResolvedValue(create(CreateTaskResponseSchema, { task: task({ id: 'new', ref: 'TSK-2036', title: 'Print pick lists' }) }));
    const { result } = renderHook(() => useCreateTask(), { wrapper: hookWrapper(api, queryClient) });

    const created = await result.current({ title: 'Print pick lists', channelId: 'ch-q4', ownerId: 'user-nok', dueDate: '2026-10-20' });

    const request = api.tasks.createTask.mock.calls[0]?.[0];
    expect(request).toMatchObject({ channelId: 'ch-q4', title: 'Print pick lists', assigneeId: 'user-nok' });
    expect(request?.source).toBeUndefined();
    expect(request?.state).toBeUndefined();
    // Midnight Tue 20 Oct in Asia/Bangkok is 17:00 UTC the day before.
    expect(request?.dueAt && new Date(Number(request.dueAt.seconds) * 1000).toISOString()).toBe('2026-10-19T17:00:00.000Z');
    expect(created).toMatchObject({ id: 'new', ref: 'TSK-2036' });
    // It enters the workspace list and its channel's list once, at the top.
    expect(queryClient.getQueryData<TaskView[]>(keys.tasks())?.map((t) => t.id)).toEqual(['new', 'existing']);
    expect(queryClient.getQueryData<TaskView[]>(keys.channelTasks('ch-q4'))?.map((t) => t.id)).toEqual(['new', 'existing']);
  });

  it('leaves out the owner and the due date a person did not choose', async () => {
    const api = createStubApi();
    api.tasks.createTask.mockResolvedValue(create(CreateTaskResponseSchema, { task: task({ id: 'new' }) }));
    const { result } = renderHook(() => useCreateTask(), { wrapper: hookWrapper(api, createTestQueryClient()) });

    await result.current({ title: 'No owner, no date', channelId: 'ch-q4', ownerId: null, dueDate: null });

    expect(api.tasks.createTask.mock.calls[0]?.[0]).toEqual({ channelId: 'ch-q4', title: 'No owner, no date' });
  });

  it('rejects with the server error and writes nothing when the request fails', async () => {
    const api = createStubApi();
    const queryClient = createTestQueryClient();
    queryClient.setQueryData(keys.tasks(), []);
    const { result } = renderHook(() => useCreateTask(), { wrapper: hookWrapper(api, queryClient) });

    await expect(result.current({ title: 'Doomed', channelId: 'ch-q4', ownerId: null, dueDate: null })).rejects.toThrow('Unexpected call');

    expect(queryClient.getQueryData(keys.tasks())).toEqual([]);
  });
});
