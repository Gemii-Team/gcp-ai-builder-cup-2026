import { create } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import { ConfirmDraftResponseSchema, MoveTaskResponseSchema, TaskState, TaskStatus } from '@kiwi/api-client';
import { cleanup, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { ANNOUNCE, cell, endBoardDay, MEETING, openBoard, RENEW, startBoardDay, TASKS } from '../../testing/boardWorld';
import { at, frame, task } from '../../testing/fixtures';
import { createStubApi } from '../../testing/stubApi';
import { stubViewport } from '../../testing/viewport';

type AnimateStub = Mock<(this: Element, keyframes: Keyframe[], options: KeyframeAnimationOptions) => Animation>;

let animate: AnimateStub;
/** Every animation the stub handed out, with its `cancel` spy. */
let cancels: Mock<() => void>[];

// jsdom lays nothing out, so a card's place is a function of the cell it sits in (and its row there), the way a layout
// would put it: To do 100px down, Doing 300px, Done 500px, rows 10px apart, and a draft in the band 50px.
const CELL_TOP = { todo: 100, doing: 300, done: 500 } as const;

function placeOf(element: Element): number {
  const status = element.closest<HTMLElement>('[data-status]')?.dataset.status;
  if (status === 'todo' || status === 'doing' || status === 'done') {
    return CELL_TOP[status] + 10 * [...(element.parentElement?.children ?? [])].indexOf(element);
  }
  return element.closest('[data-drafts-list]') === null ? 0 : 50;
}

beforeEach(() => {
  startBoardDay();
  cancels = [];
  animate = vi.fn(function () {
    const cancel = vi.fn<() => void>();
    cancels.push(cancel);
    return { cancel } as unknown as Animation;
  });
  Object.defineProperty(Element.prototype, 'animate', { value: animate, configurable: true, writable: true });
  vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
    const top = this.hasAttribute('data-task-id') ? placeOf(this) : 0;
    return { x: 0, y: top, left: 0, top, right: 0, bottom: top, width: 0, height: 0, toJSON: () => ({}) };
  });
});

afterEach(() => {
  Reflect.deleteProperty(Element.prototype, 'animate');
  endBoardDay();
});

const EXPO = 'cubic-bezier(.16,1,.3,1)';
const GLIDE = { duration: 400, easing: EXPO };

/** The glides of one task's card: the keyframes and options of every `animate` call made on it. */
function glidesOf(id: string) {
  return animate.mock.calls.flatMap((call, index) => (animate.mock.contexts[index] instanceof HTMLElement && animate.mock.contexts[index].dataset.taskId === id && animate.mock.contexts[index].hasAttribute('data-glide') ? [call] : []));
}

const moveTo = async (user: ReturnType<typeof openBoard>['user'], code: string, title: string, status: string) => {
  await user.click(within(screen.getByRole('heading', { level: 3, name: title }).closest('li') ?? document.body).getByRole('button', { name: new RegExp(`^Move ${code}`) }));
  await user.click(within(screen.getByRole('menu')).getByRole('menuitemradio', { name: status }));
};

describe('cards glide when they change cell', () => {
  it('Move_GlidesTheMovedCardFromItsOldPlaceOver400msWithTheExpoEasing', async () => {
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await screen.findByRole('heading', { level: 3, name: 'Announce the new launch date' });
    expect(glidesOf('t-announce')).toEqual([]);
    api.tasks.moveTask.mockResolvedValue(create(MoveTaskResponseSchema, { task: { ...ANNOUNCE, status: TaskStatus.DOING, position: 'a0' } }));

    await moveTo(user, 'TSK-2030', 'Announce the new launch date', 'Doing');

    await within(cell('Doing', 'Q4 launch weekly sync').getByRole('heading', { level: 3 }).closest('li') ?? document.body).findByRole('button', { name: /^Move/ });
    // From the To do cell (100) to the Doing cell (300): it starts 200px above where it ends, and settles to `none`.
    await waitFor(() => {
      expect(glidesOf('t-announce')).toEqual([[[{ transform: 'translate(0px, -200px)' }, { transform: 'none' }], GLIDE]]);
    });
    // The card below it moves up a row; a card in another lane does not move at all.
    expect(glidesOf('t-ratecard')).toEqual([[[{ transform: 'translate(0px, 10px)' }, { transform: 'none' }], GLIDE]]);
    expect(glidesOf('t-renew')).toEqual([]);
    // The server's answer changes nothing about which cell holds the card: it does not glide a second time.
    await waitFor(() => {
      expect(api.tasks.moveTask).toHaveBeenCalledTimes(1);
    });
    expect(glidesOf('t-announce')).toHaveLength(1);
  });

  it('Move_ReducedMotionAnimatesNothing', async () => {
    stubViewport(1280, true);
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await screen.findByRole('heading', { level: 3, name: 'Announce the new launch date' });
    api.tasks.moveTask.mockResolvedValue(create(MoveTaskResponseSchema, { task: { ...ANNOUNCE, status: TaskStatus.DOING, position: 'a0' } }));

    await moveTo(user, 'TSK-2030', 'Announce the new launch date', 'Doing');

    expect(await cell('Doing', 'Q4 launch weekly sync').findByRole('heading', { level: 3, name: 'Announce the new launch date' })).toBeInTheDocument();
    expect(animate).not.toHaveBeenCalled();
  });

  it('TaskChanged_FromAnotherClientGlidesTheCard', async () => {
    const { client } = openBoard();
    await screen.findByRole('heading', { level: 3, name: 'Announce the new launch date' });

    client.emit(frame({ case: 'taskChanged', value: { ...ANNOUNCE, status: TaskStatus.DONE, doneAt: timestampFromDate(at('14:41')) } }));

    // To do (100) to Done (500).
    await waitFor(() => {
      expect(glidesOf('t-announce')).toEqual([[[{ transform: 'translate(0px, -400px)' }, { transform: 'none' }], GLIDE]]);
    });
  });

  it('ConfirmedCard_GlidesFromTheDraftItCameFrom', async () => {
    const draft = task({ id: 'draft-a', ref: 'TSK-2031', title: 'Send the quotation to ACME', state: TaskState.DRAFT, assigneeId: 'user-nok', position: 'a5', source: { ...MEETING, anchor: '21', offsetMs: 1307000n } });
    const api = createStubApi();
    const { user } = openBoard({ tasks: [...TASKS, draft] }, api);
    await screen.findByRole('heading', { level: 3, name: 'Announce the new launch date' });
    api.tasks.confirmDraft.mockResolvedValue(create(ConfirmDraftResponseSchema, { task: { ...draft, state: TaskState.CONFIRMED, position: 'Zz' } }));

    await user.click(within(screen.getByRole('complementary', { name: 'AI drafts' })).getByRole('button', { name: 'Confirm: Send the quotation to ACME' }));

    // The draft sat in the band (50) and the card lands at the top of To do (100): it comes from 50px above.
    await waitFor(() => {
      expect(glidesOf('draft-a')).toEqual([[[{ transform: 'translate(0px, -50px)' }, { transform: 'none' }], GLIDE]]);
    });
    // The cards it pushed down glide too.
    expect(glidesOf('t-announce')).toEqual([[[{ transform: 'translate(0px, -10px)' }, { transform: 'none' }], GLIDE]]);
    expect(glidesOf('t-renew')).toEqual([]);
  });

  it('opening the New task form moves no card: nothing glides', async () => {
    const { user } = openBoard();
    await screen.findByRole('heading', { level: 3, name: 'Announce the new launch date' });

    await user.click(screen.getAllByRole('button', { name: 'New task' })[0] ?? document.body);

    expect(await screen.findByRole('form', { name: 'New task' })).toBeInTheDocument();
    expect(animate).not.toHaveBeenCalled();
  });

  it('cancels a glide still running when the screen goes away, and starts none after', async () => {
    const api = createStubApi();
    const { user, client } = openBoard({}, api);
    await screen.findByRole('heading', { level: 3, name: 'Announce the new launch date' });
    api.tasks.moveTask.mockResolvedValue(create(MoveTaskResponseSchema, { task: { ...ANNOUNCE, status: TaskStatus.DOING, position: 'a0' } }));
    await moveTo(user, 'TSK-2030', 'Announce the new launch date', 'Doing');
    await waitFor(() => {
      expect(glidesOf('t-announce')).toHaveLength(1);
    });
    const started = animate.mock.calls.length;
    expect(started).toBeGreaterThan(0);
    expect(cancels.every((cancel) => cancel.mock.calls.length === 0)).toBe(true);

    cleanup();

    // Every glide of the card is cancelled with the screen, so none is left to run over whatever replaces it.
    expect(cancels.filter((cancel) => cancel.mock.calls.length > 0).length).toBe(started);
    client.emit(frame({ case: 'taskChanged', value: { ...RENEW, status: TaskStatus.DONE, doneAt: timestampFromDate(at('14:41')) } }));
    expect(animate).toHaveBeenCalledTimes(started);
  });
});
