import { create } from '@bufbuild/protobuf';
import { timestampFromDate } from '@bufbuild/protobuf/wkt';
import { ConfirmDraftResponseSchema, ExcerptSchema, GetTranscriptExcerptResponseSchema, MoveTaskResponseSchema, TaskState, TaskStatus } from '@kiwi/api-client';
import { cleanup, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock, type MockInstance } from 'vitest';
import { ANNOUNCE, cell, endBoardDay, FORKLIFT, MEETING, openBoard, RATE_CARD, RENEW, startBoardDay, TASKS } from '../../testing/boardWorld';
import { at, frame, task } from '../../testing/fixtures';
import { createStubApi } from '../../testing/stubApi';
import { stubViewport } from '../../testing/viewport';

type AnimateStub = Mock<(this: Element, keyframes: Keyframe[], options: KeyframeAnimationOptions) => Animation>;

let animate: AnimateStub;
/** The `cancel` spy of every animation the stub handed out, in order. */
let cancels: Mock<() => void>[];
/** Every `getBoundingClientRect` call, to see what was measured. */
let rects: MockInstance<Element['getBoundingClientRect']>;

// jsdom lays nothing out, so the stub is the layout: a card's place is the sum of the facts that move it. A cell puts it
// 100, 300 or 500px down and each row 10px lower; each lane before it in the document, 40px; the New task form open
// above the lanes, 200px (for every card); each source excerpt open before it in the document, 120px. A draft in the
// band is 50px down. Several of these change with no card changing cell, which is what the gate must tell apart.
const CELL_TOP = { todo: 100, doing: 300, done: 500 } as const;

function placeOf(element: Element): number {
  const form = document.querySelector('.bd-nfw') === null ? 0 : 200;
  let excerpts = 0;
  for (const open of document.querySelectorAll('.kiwi-ex')) {
    if (!open.contains(element) && (open.compareDocumentPosition(element) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0) {
      excerpts += 120;
    }
  }
  const status = element.closest<HTMLElement>('[data-status]')?.dataset.status;
  if (status === 'todo' || status === 'doing' || status === 'done') {
    const lane = [...document.querySelectorAll('.bd-lane')].findIndex((candidate) => candidate.contains(element));
    return form + excerpts + CELL_TOP[status] + 10 * [...(element.parentElement?.children ?? [])].indexOf(element) + 40 * lane;
  }
  return form + (element.closest('[data-drafts-list]') === null ? 0 : 50);
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
  rects = vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
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
const NONE = { transform: 'none' };
const ANNOUNCING = 'Announce the new launch date';

/** The cards that were animated, by task id: the keyframes and options of every `animate` call made on a card. */
function glides(): Record<string, [Keyframe[], KeyframeAnimationOptions][]> {
  const by: Record<string, [Keyframe[], KeyframeAnimationOptions][]> = {};
  animate.mock.calls.forEach((call, index) => {
    const context = animate.mock.contexts[index];
    if (context instanceof HTMLElement && context.hasAttribute('data-glide')) {
      (by[context.dataset.taskId ?? ''] ??= []).push([call[0], call[1]]);
    }
  });
  return by;
}
const glidesOf = (id: string) => glides()[id] ?? [];

const moveTo = async (user: ReturnType<typeof openBoard>['user'], code: string, title: string, status: string) => {
  await user.click(within(screen.getByRole('heading', { level: 3, name: title }).closest('li') ?? document.body).getByRole('button', { name: new RegExp(`^Move ${code}`) }));
  await user.click(within(screen.getByRole('menu')).getByRole('menuitemradio', { name: status }));
};

const answer = (moved: typeof ANNOUNCE, status: TaskStatus, position = 'a0') => create(MoveTaskResponseSchema, { task: { ...moved, status, position } });

describe('cards glide when they change cell', () => {
  it('Move_GlidesTheMovedCardFromItsOldPlaceOver400msWithTheExpoEasing', async () => {
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await screen.findByRole('heading', { level: 3, name: ANNOUNCING });
    expect(glides()).toEqual({});
    api.tasks.moveTask.mockResolvedValue(answer(ANNOUNCE, TaskStatus.DOING));

    await moveTo(user, 'TSK-2030', ANNOUNCING, 'Doing');

    // From the To do cell (100) to the Doing cell (300): it starts 200px above where it ends, and settles to `none`.
    await waitFor(() => {
      expect(glidesOf('t-announce')).toEqual([[[{ transform: 'translate(0px, -200px)' }, NONE], GLIDE]]);
    });
    // The card below it moves up a row; a card in another lane does not move at all.
    expect(glidesOf('t-ratecard')).toEqual([[[{ transform: 'translate(0px, 10px)' }, NONE], GLIDE]]);
    expect(glidesOf('t-renew')).toEqual([]);
  });

  it('Move_ReducedMotionAnimatesNothing', async () => {
    stubViewport(1280, true);
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await screen.findByRole('heading', { level: 3, name: ANNOUNCING });
    api.tasks.moveTask.mockResolvedValue(answer(ANNOUNCE, TaskStatus.DOING));

    await moveTo(user, 'TSK-2030', ANNOUNCING, 'Doing');

    expect(await cell('Doing', 'Q4 launch weekly sync').findByRole('heading', { level: 3, name: ANNOUNCING })).toBeInTheDocument();
    expect(animate).not.toHaveBeenCalled();
    // Nothing is measured either, so these users pay for no forced layout: no card was asked where it is.
    const measured = rects.mock.contexts.filter((context) => context instanceof Element && context.hasAttribute('data-task-id'));
    expect(measured).toEqual([]);
  });

  it('TaskChanged_FromAnotherClientGlidesTheCard', async () => {
    const { client } = openBoard();
    await screen.findByRole('heading', { level: 3, name: ANNOUNCING });

    client.emit(frame({ case: 'taskChanged', value: { ...ANNOUNCE, status: TaskStatus.DONE, doneAt: timestampFromDate(at('14:41')) } }));

    // To do (100) to Done (500).
    await waitFor(() => {
      expect(glidesOf('t-announce')).toEqual([[[{ transform: 'translate(0px, -400px)' }, NONE], GLIDE]]);
    });
  });

  it('ConfirmedCard_GlidesFromTheDraftItCameFrom', async () => {
    const draft = task({ id: 'draft-a', ref: 'TSK-2031', title: 'Send the quotation to ACME', state: TaskState.DRAFT, assigneeId: 'user-nok', position: 'a5', source: { ...MEETING, anchor: '21', offsetMs: 1307000n } });
    const api = createStubApi();
    const { user } = openBoard({ tasks: [...TASKS, draft] }, api);
    await screen.findByRole('heading', { level: 3, name: ANNOUNCING });
    api.tasks.confirmDraft.mockResolvedValue(create(ConfirmDraftResponseSchema, { task: { ...draft, state: TaskState.CONFIRMED, position: 'Zz' } }));

    await user.click(within(screen.getByRole('complementary', { name: 'AI drafts' })).getByRole('button', { name: 'Confirm: Send the quotation to ACME' }));

    // The draft sat in the band (50) and the card lands at the top of To do in its lane (140): it comes from 90px above.
    await waitFor(() => {
      expect(glidesOf('draft-a')).toEqual([[[{ transform: 'translate(0px, -90px)' }, NONE], GLIDE]]);
    });
    // The cards it pushed down glide too.
    expect(glidesOf('t-announce')).toEqual([[[{ transform: 'translate(0px, -10px)' }, NONE], GLIDE]]);
    expect(glidesOf('t-renew')).toEqual([]);
  });
});

describe('what does not glide', () => {
  it('Gate_OpeningTheNewTaskFormShiftsEveryCardAndNoCardGlides', async () => {
    const { user } = openBoard();
    await screen.findByRole('heading', { level: 3, name: ANNOUNCING });
    const before = placeOf(screen.getByRole('heading', { level: 3, name: ANNOUNCING }).closest('li') as Element);

    await user.click(screen.getAllByRole('button', { name: 'New task' })[0] ?? document.body);

    expect(await screen.findByRole('form', { name: 'New task' })).toBeInTheDocument();
    // The stub really moved the cards (200px down)...
    expect(placeOf(screen.getByRole('heading', { level: 3, name: ANNOUNCING }).closest('li') as Element)).toBe(before + 200);
    // ...and none of them glided.
    expect(animate).not.toHaveBeenCalled();
  });

  it('Gate_AChannelFilterShiftsTheCardsThatRemainAndNoCardGlides', async () => {
    const { user } = openBoard();
    await screen.findByRole('heading', { level: 3, name: ANNOUNCING });
    const forklift = () => screen.getByRole('heading', { level: 3, name: 'Book two extra forklift shifts' }).closest('li') as Element;
    const before = placeOf(forklift());

    await user.selectOptions(screen.getByRole('combobox', { name: 'Channel' }), 'ch-ops');

    await waitFor(() => {
      expect(screen.queryByText(ANNOUNCING)).not.toBeInTheDocument();
    });
    // The lanes above it are gone, so it sits two lanes (80px) higher: it moved, and nothing glided.
    expect(placeOf(forklift())).toBe(before - 80);
    expect(animate).not.toHaveBeenCalled();
    // Clearing the filter brings cards back and shifts the ones that stayed: still nothing glides.
    await user.selectOptions(screen.getByRole('combobox', { name: 'Channel' }), '');
    await screen.findByRole('heading', { level: 3, name: ANNOUNCING });
    expect(animate).not.toHaveBeenCalled();
  });

  it('Gate_TheServerEchoOfAMoveThatReordersANeighbourGlidesNothingAgain', async () => {
    const waiting = task({ id: 't-waiting', ref: 'TSK-2019', title: 'Already doing', status: TaskStatus.DOING, position: 'a0', source: MEETING });
    const api = createStubApi();
    const { user } = openBoard({ tasks: [...TASKS, waiting] }, api);
    await screen.findByRole('heading', { level: 3, name: ANNOUNCING });
    const titles = () => within(cell('Doing', 'Q4 launch weekly sync').getByRole('list')).getAllByRole('heading', { level: 3 }).map((heading) => heading.textContent);
    // The server puts the card BELOW the one already there (the optimistic top spot was a guess): no cell changes, but
    // both cards change row.
    api.tasks.moveTask.mockResolvedValue(answer(ANNOUNCE, TaskStatus.DOING, 'a9'));

    await moveTo(user, 'TSK-2030', ANNOUNCING, 'Doing');

    await waitFor(() => {
      expect(glidesOf('t-announce')).toHaveLength(1);
    });
    await waitFor(() => {
      expect(titles()).toEqual(['Already doing', ANNOUNCING]);
    });
    // One glide per card, at the move; the echo reordered them in the stub's layout and glided neither again.
    expect(glidesOf('t-announce')).toHaveLength(1);
    expect(glidesOf('t-waiting')).toHaveLength(1);
  });

  it('Snapshot_ALayoutShiftThatNoBoardRenderCausedIsNotMistakenForAMove', async () => {
    const api = createStubApi();
    const { user } = openBoard({}, api);
    await screen.findByRole('heading', { level: 3, name: ANNOUNCING });
    api.meetings.getTranscriptExcerpt.mockResolvedValue(
      create(GetTranscriptExcerptResponseSchema, { excerpt: create(ExcerptSchema, { meetingId: 'meeting-1', title: 'Q4 launch weekly sync', entries: [{ idx: 12, text: 'Move the launch', startMs: 761000n }] }) }),
    );
    // An excerpt opens inside the first card: a child-only change, no render of the Board, that pushes every card after it
    // down by 120px in the stub's layout.
    await user.click(within(screen.getByRole('heading', { level: 3, name: ANNOUNCING }).closest('li') as HTMLElement).getByRole('button', { name: 'Transcript at 0:12:41' }));
    await screen.findByRole('group', { name: 'Transcript excerpt at 0:12:41' });
    expect(placeOf(screen.getByRole('heading', { level: 3, name: 'Decide whether to renew ACME' }).closest('li') as Element)).toBe(100 + 40 * 2 + 120 + 0);
    api.tasks.moveTask.mockResolvedValue(answer(FORKLIFT, TaskStatus.DOING));

    await moveTo(user, 'TSK-2028', 'Book two extra forklift shifts', 'Doing');

    // The moved card glides; the cards below the excerpt, which did not move, do not glide by the excerpt's height.
    await waitFor(() => {
      expect(glidesOf('t-forklift')).toEqual([[[{ transform: 'translate(0px, -200px)' }, NONE], GLIDE]]);
    });
    expect(glidesOf('t-renew')).toEqual([]);
    expect(glidesOf('t-ratecard')).toEqual([]);
    expect(glidesOf('t-announce')).toEqual([]);
  });
});

describe('a glide that is overtaken or abandoned', () => {
  it('Supersede_ASecondMoveWhileTheFirstGlidesCancelsItAndStartsNewOnes', async () => {
    const api = createStubApi();
    const { user, client } = openBoard({}, api);
    await screen.findByRole('heading', { level: 3, name: ANNOUNCING });
    api.tasks.moveTask.mockResolvedValue(answer(ANNOUNCE, TaskStatus.DOING));
    await moveTo(user, 'TSK-2030', ANNOUNCING, 'Doing');
    await waitFor(() => {
      expect(glidesOf('t-announce')).toHaveLength(1);
    });
    const firstGlides = animate.mock.calls.length;
    expect(cancels.slice(0, firstGlides).every((cancel) => cancel.mock.calls.length === 0)).toBe(true);

    // Another client moves a second card while the first glides (the stub never finishes an animation).
    client.emit(frame({ case: 'taskChanged', value: { ...RATE_CARD, status: TaskStatus.DONE, doneAt: timestampFromDate(at('14:42')) } }));

    await waitFor(() => {
      expect(animate.mock.calls.length).toBeGreaterThan(firstGlides);
    });
    expect(cancels.slice(0, firstGlides).every((cancel) => cancel.mock.calls.length === 1)).toBe(true);
    expect(glidesOf('t-ratecard')).toHaveLength(2);
  });

  it('cancels a glide still running when the screen goes away, and starts none after', async () => {
    const api = createStubApi();
    const { user, client } = openBoard({}, api);
    await screen.findByRole('heading', { level: 3, name: ANNOUNCING });
    api.tasks.moveTask.mockResolvedValue(answer(ANNOUNCE, TaskStatus.DOING));
    await moveTo(user, 'TSK-2030', ANNOUNCING, 'Doing');
    await waitFor(() => {
      expect(glidesOf('t-announce')).toHaveLength(1);
    });
    const started = animate.mock.calls.length;
    expect(cancels.every((cancel) => cancel.mock.calls.length === 0)).toBe(true);

    cleanup();

    expect(cancels.filter((cancel) => cancel.mock.calls.length > 0)).toHaveLength(started);
    client.emit(frame({ case: 'taskChanged', value: { ...RENEW, status: TaskStatus.DONE, doneAt: timestampFromDate(at('14:41')) } }));
    expect(animate).toHaveBeenCalledTimes(started);
  });
});
