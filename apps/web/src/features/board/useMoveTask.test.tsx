import { Code, ConnectError } from '@connectrpc/connect';
import { create } from '@bufbuild/protobuf';
import { ListTasksResponseSchema, MoveTaskResponseSchema, TaskState, TaskStatus, type MoveTaskResponse, type Task } from '@kiwi/api-client';
import type { QueryClient } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { mapTask } from '../../api/mappers';
import { keys } from '../../api/queries';
import type { TaskView } from '../../api/types';
import { task } from '../../testing/fixtures';
import { createTestQueryClient, hookWrapper } from '../../testing/render';
import { createStubApi } from '../../testing/stubApi';
import { useMoveTask } from './useMoveTask';

const MOVING = task({ id: 'moving', title: 'Moving', status: TaskStatus.TODO, position: 'a0' });

function setup(tasks: Task[]) {
  const api = createStubApi();
  const queryClient = createTestQueryClient();
  queryClient.setQueryData(keys.tasks(), tasks.map(mapTask));
  return { api, queryClient, wrapper: hookWrapper(api, queryClient) };
}

const cached = (queryClient: QueryClient, id = 'moving') => queryClient.getQueryData<TaskView[]>(keys.tasks())?.find((t) => t.id === id);

describe('useMoveTask', () => {
  it('useMoveTask_PlacesCardAtTopOfDestinationStatus', async () => {
    const { api, queryClient, wrapper } = setup([
      MOVING,
      task({ id: 'doing-b', status: TaskStatus.DOING, position: 'a1' }),
      task({ id: 'doing-a', status: TaskStatus.DOING, position: 'a0' }),
      // None of these is the top of the card's own destination: another channel's card, a draft, a card of another status.
      task({ id: 'other-channel', channelId: 'ch-ops', status: TaskStatus.DOING, position: '' }),
      task({ id: 'draft', status: TaskStatus.DOING, state: TaskState.DRAFT, position: '' }),
      task({ id: 'done', status: TaskStatus.DONE, position: '' }),
    ]);
    api.tasks.moveTask.mockResolvedValue(create(MoveTaskResponseSchema, { task: { ...MOVING, status: TaskStatus.DOING, position: 'Zz' } }));
    const { result } = renderHook(() => useMoveTask('moving'), { wrapper });

    await result.current('doing');

    // `before_id` is the card now first there: the moved card goes above it. No `after_id`.
    expect(api.tasks.moveTask).toHaveBeenCalledTimes(1);
    expect(api.tasks.moveTask.mock.calls[0]?.[0]).toEqual({ id: 'moving', status: TaskStatus.DOING, beforeId: 'doing-a' });
    // The server's answer is what stays in the cache.
    expect(cached(queryClient)).toMatchObject({ status: 'doing', position: 'Zz' });
  });

  it('sends neither neighbour for an empty destination', async () => {
    const { api, wrapper } = setup([MOVING, task({ id: 'doing-other-channel', channelId: 'ch-ops', status: TaskStatus.DOING, position: 'a0' })]);
    api.tasks.moveTask.mockResolvedValue(create(MoveTaskResponseSchema, { task: { ...MOVING, status: TaskStatus.DOING, position: 'a0' } }));
    const { result } = renderHook(() => useMoveTask('moving'), { wrapper });

    await result.current('doing');

    expect(api.tasks.moveTask.mock.calls[0]?.[0]).toEqual({ id: 'moving', status: TaskStatus.DOING });
  });

  it('moves the card in the cache at once, before the server has answered', async () => {
    const { api, queryClient, wrapper } = setup([MOVING, task({ id: 'doing-a', status: TaskStatus.DOING, position: 'a0' })]);
    const answer = Promise.withResolvers<MoveTaskResponse>();
    api.tasks.moveTask.mockReturnValue(answer.promise);
    const { result } = renderHook(() => useMoveTask('moving'), { wrapper });

    const moving = result.current('done');

    await waitFor(() => {
      expect(cached(queryClient)).toMatchObject({ status: 'done', position: '' });
    });
    expect(cached(queryClient)?.doneAt).not.toBeNull();
    answer.resolve(create(MoveTaskResponseSchema, { task: { ...MOVING, status: TaskStatus.DONE, position: 'a0' } }));
    await moving;
    expect(cached(queryClient)).toMatchObject({ status: 'done', position: 'a0' });
  });

  it('useMoveTask_RollsBackOnServerError', async () => {
    const { api, queryClient, wrapper } = setup([MOVING, task({ id: 'doing-a', status: TaskStatus.DOING, position: 'a0' })]);
    const before = cached(queryClient);
    api.tasks.moveTask.mockRejectedValue(new ConnectError('down', Code.Unavailable));
    const { result } = renderHook(() => useMoveTask('moving'), { wrapper });

    await expect(result.current('doing')).rejects.toMatchObject({ code: Code.Unavailable });

    expect(api.tasks.moveTask).toHaveBeenCalledTimes(1);
    expect(cached(queryClient)).toEqual(before);
    expect(cached(queryClient)).toMatchObject({ status: 'todo', position: 'a0' });
  });

  it('loads the tasks again and tries once more when the lane changed underneath it (Aborted)', async () => {
    const { api, queryClient, wrapper } = setup([MOVING, task({ id: 'doing-a', status: TaskStatus.DOING, position: 'a1' })]);
    // Someone moved doing-a away and put doing-b first while the request was on its way.
    api.tasks.listTasks.mockResolvedValue(create(ListTasksResponseSchema, { tasks: [MOVING, task({ id: 'doing-b', status: TaskStatus.DOING, position: 'a0' })] }));
    api.tasks.moveTask
      .mockRejectedValueOnce(new ConnectError('lane changed', Code.Aborted))
      .mockResolvedValueOnce(create(MoveTaskResponseSchema, { task: { ...MOVING, status: TaskStatus.DOING, position: 'Zz' } }));
    const { result } = renderHook(() => useMoveTask('moving'), { wrapper });

    await result.current('doing');

    expect(api.tasks.moveTask.mock.calls.map((call) => call[0])).toEqual([
      { id: 'moving', status: TaskStatus.DOING, beforeId: 'doing-a' },
      { id: 'moving', status: TaskStatus.DOING, beforeId: 'doing-b' },
    ]);
    expect(cached(queryClient)).toMatchObject({ status: 'doing', position: 'Zz' });
  });

  it('rolls back and rejects when the retry fails too', async () => {
    const { api, queryClient, wrapper } = setup([MOVING]);
    api.tasks.listTasks.mockResolvedValue(create(ListTasksResponseSchema, { tasks: [MOVING] }));
    api.tasks.moveTask.mockRejectedValue(new ConnectError('lane changed', Code.Aborted));
    const { result } = renderHook(() => useMoveTask('moving'), { wrapper });

    await expect(result.current('doing')).rejects.toMatchObject({ code: Code.Aborted });

    expect(api.tasks.moveTask).toHaveBeenCalledTimes(2);
    expect(cached(queryClient)).toMatchObject({ status: 'todo', position: 'a0' });
  });
});
