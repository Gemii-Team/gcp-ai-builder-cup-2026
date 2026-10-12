// @vitest-environment jsdom
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createRef, StrictMode, useState } from 'react';
import { afterEach, describe, expect, it, vi, type Mock } from 'vitest';
import { en } from '../labels/en';
import { LabelsProvider } from '../labels';
import { ExcerptProvider, type Source } from '../slip/ExcerptContext';
import { DraftSlip, type DraftTask } from './DraftSlip';
import { DraftsBar } from './DraftsBar';
import { DraftsPanel } from './DraftsPanel';
import type { Member } from './OwnerSelect';
import { SealedSlip, type SealedTask } from './SealedSlip';

const meeting: Source = { kind: 'meeting', id: 'm1', label: 'Q4 launch weekly sync', channelId: 'c1', offsetMs: 1307000, entryIdx: 20 };
const contract: Source = { kind: 'file', id: 'f1', label: 'ACME-contract-v3.pdf', channelId: 'c1', page: 4 };

const members: Member[] = [
  { id: 'u1', name: 'Somchai W.' },
  { id: 'u2', name: 'Nok' },
  { id: 'u3', name: 'Arthit K.' },
];

const quotation: DraftTask = {
  id: 't1',
  ref: 'TSK-2031',
  title: 'Send the quotation to ACME',
  ownerId: 'u1',
  dueLabel: 'Fri 16 Oct',
  source: meeting,
};
const slides: DraftTask = {
  id: 't2',
  ref: 'TSK-2032',
  title: 'Update the onboarding slides',
  ownerId: 'u2',
  dueLabel: 'Thu 15 Oct',
  source: { ...meeting, offsetMs: 1802000 },
};
const warehouse: DraftTask = {
  id: 't3',
  ref: 'TSK-2033',
  title: 'Book the Bang Na warehouse slot',
  ownerId: null,
  source: { ...meeting, offsetMs: 2140000 },
};

type Patch = { title?: string; ownerId?: string | null };

type HarnessProps = {
  drafts: DraftTask[];
  sealed?: SealedTask[];
  variant?: 'panel' | 'band';
  confirm?: (id: string) => Promise<void>;
  edit?: (id: string, patch: Patch) => Promise<void>;
  /** How long the list waits after the server answers before it drops the draft, like a realtime event that arrives late. */
  listLagMs?: number;
};

const sealedFrom = (draft: DraftTask): SealedTask => ({
  id: draft.id,
  ref: draft.ref,
  title: draft.title,
  ownerName: members.find((member) => member.id === draft.ownerId)?.name,
  dueLabel: draft.dueLabel,
  source: draft.source,
  confirmedBy: 'Pimchanok S.',
  confirmedAt: '2026-10-12T07:52:00Z',
});

/** Holds the drafts the way the app's query cache does: the list only changes when the server has said yes. */
function Harness({ drafts: initial, sealed: initialSealed = [], variant = 'panel', confirm, edit, listLagMs = 0 }: HarnessProps) {
  const [drafts, setDrafts] = useState(initial);
  const [sealed, setSealed] = useState(initialSealed);
  return (
    <ExcerptProvider load={() => Promise.reject(new Error('unused'))}>
      <DraftsPanel
        variant={variant}
        drafts={drafts}
        sealed={sealed}
        members={members}
        counts={{ todo: 7, doing: 4, done: 12 }}
        onConfirm={async (id) => {
          await confirm?.(id);
          const draft = drafts.find((candidate) => candidate.id === id);
          const apply = () => {
            setDrafts((current) => current.filter((candidate) => candidate.id !== id));
            if (draft !== undefined) {
              setSealed((current) => [sealedFrom(draft), ...current]);
            }
          };
          if (listLagMs > 0) {
            setTimeout(apply, listLagMs);
          } else {
            apply();
          }
        }}
        onEdit={async (id, patch) => {
          await edit?.(id, patch);
          setDrafts((current) => current.map((candidate) => (candidate.id === id ? { ...candidate, ...patch } : candidate)));
        }}
      />
    </ExcerptProvider>
  );
}

function renderPanel(props: HarnessProps) {
  const confirm = vi.fn(props.confirm ?? (() => Promise.resolve()));
  const edit = vi.fn(props.edit ?? (() => Promise.resolve()));
  const view = render(<Harness {...props} confirm={confirm} edit={edit} />);
  return { ...view, confirm, edit };
}

const draftItem = (title: string) => {
  const heading = screen.getByRole('heading', { name: title });
  const item = heading.closest('li');
  if (item === null) {
    throw new Error(`"${title}" is not in a list item`);
  }
  return within(item);
};

function stubReducedMotion(matches: boolean) {
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches: matches && query.includes('prefers-reduced-motion'),
    media: query,
    addEventListener: () => undefined,
    removeEventListener: () => undefined,
  }));
}

type AnimateStub = Mock<(keyframes: Keyframe[], options: KeyframeAnimationOptions) => Animation>;

/** jsdom has no Web Animations: the stub records each call and the element it was made on. */
function stubAnimate(): AnimateStub {
  const animate: AnimateStub = vi.fn((keyframes, options) => ({ keyframes, options }) as unknown as Animation);
  Object.defineProperty(Element.prototype, 'animate', { configurable: true, writable: true, value: animate });
  return animate;
}

const animationsOn = (animate: AnimateStub, element: Element) =>
  animate.mock.calls.filter((_, index) => animate.mock.contexts[index] === element);

/**
 * jsdom lays nothing out. Each `data-flip` item sits 120px under the one before it, the confirmed list from the top and
 * the drafts list from 400px, so removing the first draft moves every other one up by 120px.
 */
function stubLayout() {
  const rect = (left: number, top: number, width: number, height: number) =>
    ({ left, top, width, height, right: left + width, bottom: top + height, x: left, y: top, toJSON: () => ({}) }) as DOMRect;
  const topOf = (item: Element) => {
    const list = item.parentElement;
    return (list?.hasAttribute('data-sealed-list') ? 0 : 400) + 120 * Array.from(list?.children ?? []).indexOf(item);
  };
  vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
    if (this.hasAttribute('data-flip')) {
      return rect(0, topOf(this), 300, 100);
    }
    const item = this.closest('[data-flip]');
    return item === null ? rect(0, 0, 0, 0) : rect(8, topOf(item) + 60, 284, 40);
  });
}

/** Lets pending promises and `ms` of fake time run inside `act`. */
const settle = (ms: number) =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, 'animate');
});

describe('OwnerSelect', () => {
  it('OwnerSelect_NoOwnerDisablesConfirmWithReason', () => {
    renderPanel({ drafts: [warehouse] });
    const slip = draftItem(warehouse.title);
    const owner = slip.getByRole('combobox', { name: 'Owner' });
    expect(owner).toHaveValue('');
    expect(owner).toHaveAttribute('data-warn');
    expect(within(owner).getByRole('option', { name: 'Choose an owner' })).toBeInTheDocument();
    expect(within(owner).getAllByRole('option').map((option) => option.textContent)).toEqual([
      'Choose an owner',
      'Somchai W.',
      'Nok',
      'Arthit K.',
    ]);
    expect(owner.closest('li')?.querySelector('[data-icon="warning"]')).not.toBeNull();
    const confirm = slip.getByRole('button', { name: `Confirm: ${warehouse.title}` });
    expect(confirm).toBeDisabled();
    expect(confirm).toHaveAccessibleDescription('Choose an owner first.');
  });

  it('OwnerSelect_ChoosingAnOwnerEnablesConfirm', async () => {
    const user = userEvent.setup();
    const { edit } = renderPanel({ drafts: [warehouse] });
    const slip = draftItem(warehouse.title);
    await user.selectOptions(slip.getByRole('combobox', { name: 'Owner' }), 'Nok');
    expect(edit).toHaveBeenCalledExactlyOnceWith('t3', { ownerId: 'u2' });
    const owner = slip.getByRole('combobox', { name: 'Owner' });
    expect(owner).toHaveValue('u2');
    expect(owner).not.toHaveAttribute('data-warn');
    expect(owner.closest('li')?.querySelector('[data-icon="warning"]')).toBeNull();
    const confirm = slip.getByRole('button', { name: `Confirm: ${warehouse.title}` });
    expect(confirm).toBeEnabled();
    expect(confirm).not.toHaveAccessibleDescription('Choose an owner first.');
  });

  it('keeps the owner it was given when the server refuses the change, and says so', async () => {
    const user = userEvent.setup();
    renderPanel({ drafts: [warehouse], edit: () => Promise.reject(new Error('conflict')) });
    const slip = draftItem(warehouse.title);
    await user.selectOptions(slip.getByRole('combobox', { name: 'Owner' }), 'Nok');
    expect(await slip.findByRole('alert')).toHaveTextContent("Couldn't save the change. Try again.");
    expect(slip.getByRole('combobox', { name: 'Owner' })).toHaveValue('');
    expect(slip.getByRole('button', { name: `Confirm: ${warehouse.title}` })).toBeDisabled();
  });
});

describe('DraftSlip editing with an input method', () => {
  // The Escape that dismisses a Thai IME candidate and the Enter that commits one belong to the IME, not to the edit.
  it('DraftSlip_ImeEscapeAndEnterNeitherCancelNorSaveTheEdit', async () => {
    const user = userEvent.setup();
    const { edit } = renderPanel({ drafts: [quotation] });
    const slip = draftItem(quotation.title);
    await user.click(slip.getByRole('button', { name: `Edit: ${quotation.title}` }));
    const field = slip.getByRole('textbox', { name: 'Task title' });
    await user.clear(field);
    await user.type(field, 'ส่งใบเสนอราคา');

    fireEvent.keyDown(field, { key: 'Escape', isComposing: true, keyCode: 229 });
    fireEvent.keyDown(field, { key: 'Escape', isComposing: false, keyCode: 229 });
    fireEvent.keyDown(field, { key: 'Enter', isComposing: true, keyCode: 229 });
    fireEvent.keyDown(field, { key: 'Enter', isComposing: false, keyCode: 229 });

    expect(slip.getByRole('textbox', { name: 'Task title' })).toHaveValue('ส่งใบเสนอราคา');
    expect(edit).not.toHaveBeenCalled();

    // Outside composition the same keys work as they always did.
    await user.keyboard('{Escape}');
    expect(slip.queryByRole('textbox')).toBeNull();
    expect(edit).not.toHaveBeenCalled();
  });
});

describe('DraftSlip editing', () => {
  it('DraftSlip_EnterSavesEscapeCancelsEmptyTitleInvalid', async () => {
    const user = userEvent.setup();
    const { edit } = renderPanel({ drafts: [quotation] });
    const slip = draftItem(quotation.title);

    // Edit swaps the title for a field that holds it, selected.
    await user.click(slip.getByRole('button', { name: `Edit: ${quotation.title}` }));
    let field = slip.getByRole('textbox', { name: 'Task title' });
    expect(field).toHaveFocus();
    expect(field).toHaveValue(quotation.title);
    expect(field).toHaveProperty('selectionStart', 0);
    expect(field).toHaveProperty('selectionEnd', quotation.title.length);
    expect(screen.queryByRole('heading', { name: quotation.title })).toBeNull();

    // Escape cancels: nothing is saved and focus returns to Edit.
    await user.type(field, 'Something else');
    await user.keyboard('{Escape}');
    expect(edit).not.toHaveBeenCalled();
    expect(slip.queryByRole('textbox')).toBeNull();
    expect(screen.getByRole('heading', { name: quotation.title })).toBeInTheDocument();
    expect(slip.getByRole('button', { name: `Edit: ${quotation.title}` })).toHaveFocus();

    // An empty title is invalid: the message shows, the field keeps focus and nothing is sent.
    await user.click(slip.getByRole('button', { name: `Edit: ${quotation.title}` }));
    field = slip.getByRole('textbox', { name: 'Task title' });
    await user.clear(field);
    await user.keyboard('{Enter}');
    expect(field).toHaveAttribute('aria-invalid', 'true');
    expect(field).toHaveAccessibleDescription("Title can't be empty.");
    expect(slip.getByText("Title can't be empty.")).toBeInTheDocument();
    expect(field).toHaveFocus();
    expect(edit).not.toHaveBeenCalled();

    // Typing clears the invalid look; Enter then saves the trimmed title and returns focus to Edit.
    await user.type(field, '  Send the quotation to Globex ');
    expect(field).not.toHaveAttribute('aria-invalid');
    expect(slip.queryByText("Title can't be empty.")).toBeNull();
    await user.keyboard('{Enter}');
    expect(edit).toHaveBeenCalledExactlyOnceWith('t1', { title: 'Send the quotation to Globex' });
    expect(slip.queryByRole('textbox')).toBeNull();
    expect(screen.getByRole('heading', { name: 'Send the quotation to Globex' })).toBeInTheDocument();
    expect(slip.getByRole('button', { name: 'Edit: Send the quotation to Globex' })).toHaveFocus();
  });

  it('saves with the Save button and cancels with Cancel', async () => {
    const user = userEvent.setup();
    const { edit } = renderPanel({ drafts: [quotation] });
    const slip = draftItem(quotation.title);
    await user.click(slip.getByRole('button', { name: `Edit: ${quotation.title}` }));
    expect(slip.queryByRole('button', { name: /^Confirm/ })).toBeNull();
    await user.click(slip.getByRole('button', { name: 'Cancel' }));
    expect(edit).not.toHaveBeenCalled();
    await user.click(slip.getByRole('button', { name: `Edit: ${quotation.title}` }));
    await user.clear(slip.getByRole('textbox', { name: 'Task title' }));
    await user.type(slip.getByRole('textbox', { name: 'Task title' }), 'Renamed');
    await user.click(slip.getByRole('button', { name: 'Save' }));
    expect(edit).toHaveBeenCalledExactlyOnceWith('t1', { title: 'Renamed' });
  });
});

describe('Confirm', () => {
  it('Confirm_RejectedByServer_StaysUnsealedAndShowsError', async () => {
    const user = userEvent.setup();
    const animate = stubAnimate();
    stubLayout();
    const { confirm } = renderPanel({ drafts: [quotation, slides], confirm: () => Promise.reject(new Error('conflict')) });
    const slip = draftItem(quotation.title);
    const button = slip.getByRole('button', { name: `Confirm: ${quotation.title}` });

    await user.click(button);

    expect(confirm).toHaveBeenCalledExactlyOnceWith('t1');
    expect(await slip.findByRole('alert')).toHaveTextContent("Couldn't confirm this draft. Try again.");
    // Nothing sealed: the draft is still a draft, with its perforation, and no seal, torn edge or shadow exists.
    expect(screen.getByRole('list', { name: 'Drafts to confirm' })).toBeInTheDocument();
    expect(within(screen.getByRole('list', { name: 'Drafts to confirm' })).getAllByRole('listitem')).toHaveLength(2);
    expect(screen.queryByRole('img', { name: 'Confirmed' })).toBeNull();
    expect(document.querySelector('.torn')).toBeNull();
    expect(document.querySelector('.kiwi-perf')).not.toBeNull();
    // Nothing moved, tore or spoke before the server agreed.
    expect(document.querySelector('.kiwi-ghost')).toBeNull();
    expect(animate).not.toHaveBeenCalled();
    expect(button).toHaveFocus();
    await new Promise((resolve) => setTimeout(resolve, 80));
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
    // The same button tries again, and the error goes with the next attempt.
    expect(button).toBeEnabled();
  });

  it('clears the error and seals once the server agrees on the retry', async () => {
    const user = userEvent.setup();
    stubReducedMotion(true);
    const confirm = vi.fn().mockRejectedValueOnce(new Error('offline')).mockResolvedValue(undefined);
    renderPanel({ drafts: [quotation], confirm });
    const slip = draftItem(quotation.title);
    await user.click(slip.getByRole('button', { name: `Confirm: ${quotation.title}` }));
    expect(await slip.findByRole('alert')).toBeInTheDocument();
    await user.click(slip.getByRole('button', { name: `Confirm: ${quotation.title}` }));
    expect(await screen.findByRole('img', { name: 'Confirmed' })).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(confirm).toHaveBeenCalledTimes(2);
  });

  it('sends one request however often Confirm is pressed while the server is thinking', async () => {
    const user = userEvent.setup();
    stubReducedMotion(true);
    let answer: () => void = () => undefined;
    const confirm = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          answer = resolve;
        }),
    );
    renderPanel({ drafts: [quotation], confirm });
    const button = draftItem(quotation.title).getByRole('button', { name: `Confirm: ${quotation.title}` });
    await user.click(button);
    await user.click(button);
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(button).toHaveAttribute('aria-busy', 'true');
    answer();
    expect(await screen.findByRole('img', { name: 'Confirmed' })).toBeInTheDocument();
  });
});

describe('tear and seal', () => {
  it('TearAndSeal_WithMotion_CreatesAndRemovesAriaHiddenGhost', async () => {
    vi.useFakeTimers();
    const animate = stubAnimate();
    stubLayout();
    renderPanel({ drafts: [quotation, slides, warehouse] });
    const first = draftItem(quotation.title);
    const second = draftItem(slides.title).getByRole('heading', { name: slides.title }).closest('li');
    const stub = first.getByText('REF TSK-2031').closest('.kiwi-stub');

    fireEvent.click(first.getByRole('button', { name: `Confirm: ${quotation.title}` }));
    await settle(0);

    // The ghost is the stub's clone: out of the accessibility tree, out of the tab order, without ids.
    const ghosts = document.body.querySelectorAll<HTMLElement>(':scope > .kiwi-ghost');
    expect(ghosts).toHaveLength(1);
    const ghost = ghosts[0];
    expect(ghost).toHaveAttribute('aria-hidden', 'true');
    expect(ghost).toHaveAttribute('inert');
    expect(ghost.querySelector('[id]')).toBeNull();
    expect(ghost).toHaveTextContent('REF TSK-2031');
    expect(ghost).not.toBe(stub);
    expect(ghost.style).toMatchObject({ left: '8px', top: '460px', width: '284px', height: '40px' });

    // It slides 28px, tilts 3 degrees and fades over 400ms.
    expect(animationsOn(animate, ghost)).toEqual([
      [
        [
          { transform: 'none', opacity: 1 },
          { transform: 'translateY(28px) rotate(3deg)', opacity: 0 },
        ],
        { duration: 400, easing: 'cubic-bezier(.16,1,.3,1)', fill: 'forwards' },
      ],
    ]);

    // The seal stamps in 120ms late over 280ms, onto the slip that now sits at the head of the confirmed list.
    const confirmed = screen.getByRole('list', { name: 'Confirmed' });
    const seal = within(confirmed).getByRole('img', { name: 'Confirmed' });
    expect(animationsOn(animate, seal)).toEqual([
      [
        [
          { opacity: 0, transform: 'rotate(-18deg) scale(1.5)' },
          { opacity: 1, transform: 'rotate(-8deg) scale(1)' },
        ],
        { duration: 280, delay: 120, easing: 'cubic-bezier(.16,1,.3,1)', fill: 'backwards' },
      ],
    ]);

    // The drafts that remain glide from where they were; the sealed slip glides from the draft's place.
    const glide = (offset: number) => [
      [{ transform: `translate(0px,${offset}px)` }, { transform: 'none' }],
      { duration: 400, easing: 'cubic-bezier(.16,1,.3,1)' },
    ];
    expect(second).not.toBeNull();
    expect(animationsOn(animate, second as Element)).toEqual([glide(120)]);
    expect(animationsOn(animate, within(confirmed).getAllByRole('listitem')[0])).toEqual([glide(400)]);

    // The ghost goes when the 400ms are over, not before.
    await settle(399);
    expect(ghost.isConnected).toBe(true);
    await settle(1);
    expect(ghost.isConnected).toBe(false);
    expect(document.body.querySelector('.kiwi-ghost')).toBeNull();
  });

  it('TearAndSeal_ReducedMotion_NoGhostNoAnimate', async () => {
    const user = userEvent.setup();
    stubReducedMotion(true);
    const animate = stubAnimate();
    stubLayout();
    renderPanel({ drafts: [quotation, slides] });

    await user.click(draftItem(quotation.title).getByRole('button', { name: `Confirm: ${quotation.title}` }));

    // The slip is replaced at once, and still announced; nothing is torn or animated.
    expect(await screen.findByRole('img', { name: 'Confirmed' })).toBeInTheDocument();
    expect(document.querySelector('.kiwi-ghost')).toBeNull();
    expect(await screen.findByText('Confirmed: Send the quotation to ACME. 1 draft left.')).toBeInTheDocument();
    expect(animate).not.toHaveBeenCalled();
  });

  it('seals when the list drops the draft after the server has already answered', async () => {
    const user = userEvent.setup();
    stubReducedMotion(true);
    renderPanel({ drafts: [quotation, slides], listLagMs: 30 });
    await user.click(draftItem(quotation.title).getByRole('button', { name: `Confirm: ${quotation.title}` }));
    expect(await screen.findByRole('img', { name: 'Confirmed' })).toBeInTheDocument();
    expect(await screen.findByText('Confirmed: Send the quotation to ACME. 1 draft left.')).toBeInTheDocument();
    expect(draftItem(slides.title).getByRole('button', { name: `Confirm: ${slides.title}` })).toHaveFocus();
  });

  it('Confirm_ServerAnswersAfterUnmount_TearsAndSchedulesNothing', async () => {
    vi.useFakeTimers();
    const animate = stubAnimate();
    stubLayout();
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    let answer: () => void = () => undefined;
    const confirm = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          answer = resolve;
        }),
    );
    const { unmount } = renderPanel({ drafts: [quotation, slides], confirm });

    fireEvent.click(draftItem(quotation.title).getByRole('button', { name: `Confirm: ${quotation.title}` }));
    expect(confirm).toHaveBeenCalledExactlyOnceWith('t1');

    // The route changes while the server is still thinking; then the server says yes.
    unmount();
    answer();
    await settle(0);

    // No slip is left to tear: no ghost over the screen that replaced the panel, no animation, and nothing scheduled.
    // `play` always sets the live region and schedules its text 40ms later, so a timer count of 0 also means that no
    // state was set on the unmounted hook. This is read straight after the answer, before a ghost's own 400ms removal
    // could hide it, and again once every timer that could have been scheduled would have fired.
    const nothing = () => {
      expect(document.body.querySelector('.kiwi-ghost')).toBeNull();
      expect(animate).not.toHaveBeenCalled();
      expect(vi.getTimerCount()).toBe(0);
    };
    nothing();
    await settle(1000);
    nothing();
    expect(error).not.toHaveBeenCalled();
    expect(warn).not.toHaveBeenCalled();
  });

  it('Confirm_StillSealsUnderStrictMode', async () => {
    // The app renders in StrictMode, which mounts, unmounts and mounts every effect again: the hook's "on screen" flag
    // must survive that rehearsal, or no draft would ever tear.
    const user = userEvent.setup();
    stubReducedMotion(true);
    render(
      <StrictMode>
        <Harness drafts={[quotation, slides]} />
      </StrictMode>,
    );
    await user.click(draftItem(quotation.title).getByRole('button', { name: `Confirm: ${quotation.title}` }));
    expect(await screen.findByRole('img', { name: 'Confirmed' })).toBeInTheDocument();
    expect(await screen.findByText('Confirmed: Send the quotation to ACME. 1 draft left.')).toBeInTheDocument();
    expect(draftItem(slides.title).getByRole('button', { name: `Confirm: ${slides.title}` })).toHaveFocus();
  });

  it('Confirm_AnnouncesAndMovesFocusToNextConfirm', async () => {
    const user = userEvent.setup();
    stubReducedMotion(true);
    renderPanel({ drafts: [quotation, slides, warehouse] });
    const status = screen.getByRole('status');
    expect(status).toBeEmptyDOMElement();

    await user.click(draftItem(quotation.title).getByRole('button', { name: `Confirm: ${quotation.title}` }));
    await waitFor(() => {
      expect(status).toHaveTextContent('Confirmed: Send the quotation to ACME. 2 drafts left.');
    });
    expect(draftItem(slides.title).getByRole('button', { name: `Confirm: ${slides.title}` })).toHaveFocus();

    // The next draft has no owner yet, so its Confirm is disabled and the owner select takes the focus instead.
    await user.click(draftItem(slides.title).getByRole('button', { name: `Confirm: ${slides.title}` }));
    await waitFor(() => {
      expect(status).toHaveTextContent('Confirmed: Update the onboarding slides. 1 draft left.');
    });
    expect(draftItem(warehouse.title).getByRole('combobox', { name: 'Owner' })).toHaveFocus();

    await user.selectOptions(draftItem(warehouse.title).getByRole('combobox', { name: 'Owner' }), 'Nok');
    await user.click(draftItem(warehouse.title).getByRole('button', { name: `Confirm: ${warehouse.title}` }));
    await waitFor(() => {
      expect(status).toHaveTextContent('Confirmed: Book the Bang Na warehouse slot. All drafts confirmed.');
    });
    expect(screen.getAllByRole('img', { name: 'Confirmed' })).toHaveLength(3);
  });
});

describe('the Perforation Rule and the One Shadow Rule', () => {
  const sealed = sealedFrom(quotation);

  it('SealedSlip_HasNoPerforationAndDraftSlipDoes', () => {
    render(
      <ExcerptProvider load={() => Promise.reject(new Error('unused'))}>
        <SealedSlip {...sealed} />
        <DraftSlip draft={quotation} members={members} onEdit={() => Promise.resolve()} onConfirm={() => Promise.resolve()} />
      </ExcerptProvider>,
    );
    const torn = document.querySelector('.torn');
    expect(torn).not.toBeNull();
    expect(torn?.querySelector('.kiwi-perf')).toBeNull();
    expect(torn?.querySelector('.kiwi-stub')).toBeNull();
    expect(torn?.querySelector('.kiwi-sealed')).not.toBeNull();
    const draft = document.querySelector('.kiwi-draft');
    expect(draft?.querySelector('.kiwi-perf')).not.toBeNull();
    // Only the sealed slip is wrapped in the shadow; the draft is flat.
    expect(draft?.closest('.torn')).toBeNull();
    expect(document.querySelectorAll('.torn')).toHaveLength(1);
  });

  it('SealedSlip tells who confirmed it and when, where it came from, and its REF', () => {
    render(
      <ExcerptProvider load={() => Promise.reject(new Error('unused'))}>
        <SealedSlip {...sealed} actions={<button type="button">Move</button>} note="Added to To do" />
        <SealedSlip {...sealedFrom({ ...warehouse, ownerId: null, source: contract })} />
      </ExcerptProvider>,
    );
    const [first, second] = screen.getAllByRole('img', { name: 'Confirmed' }).map((seal) => within(seal.closest('.torn') as HTMLElement));
    expect(first.getByRole('heading', { name: quotation.title })).toBeInTheDocument();
    expect(first.getByText('Somchai W. · Fri 16 Oct')).toBeInTheDocument();
    expect(first.getByText('REF TSK-2031')).toBeInTheDocument();
    expect(first.getByText('From meeting')).toBeInTheDocument();
    expect(first.getByRole('button', { name: 'Transcript at 0:21:47' })).toHaveTextContent('0:21:47');
    expect(first.getByText('Confirmed by')).toHaveClass('sr-only');
    expect(first.getByText('Pimchanok S.')).toBeInTheDocument();
    expect(first.getByText('14:52')).toBeInTheDocument();
    expect(first.getByText('Added to To do')).toBeInTheDocument();
    expect(first.getByRole('button', { name: 'Move' })).toBeInTheDocument();
    // A document names the file, and an unowned, undated task has no meta line.
    expect(second.getByText('ACME-contract-v3.pdf')).toBeInTheDocument();
    expect(second.getByRole('button', { name: 'ACME-contract-v3.pdf, page 4' })).toBeInTheDocument();
    expect(second.queryByText(/·/)).toBeNull();
  });

  it('OneShadowRule', () => {
    const root = join(import.meta.dirname, '..');
    const stylesheets = readdirSync(root, { recursive: true, encoding: 'utf8' })
      .map((path) => path.replaceAll('\\', '/'))
      .filter((path) => path.endsWith('.css'))
      .map((path) => ({ path, css: readFileSync(join(root, path), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '') }));
    expect(stylesheets.map(({ path }) => path)).toEqual(expect.arrayContaining(['theme.css', 'slip/slip.css', 'draft/draft.css']));

    // A use is a drop-shadow() call: theme.css only declares the token, which is a definition, not a use.
    const uses = stylesheets.flatMap(({ path, css }) => [...css.matchAll(/drop-shadow\(/g)].map(() => path));
    expect(uses).toEqual(['draft/draft.css']);
    const draft = stylesheets.find(({ path }) => path === 'draft/draft.css');
    expect(draft?.css).toMatch(/\.torn\s*\{[^{}]*filter:\s*drop-shadow\(var\(--drop-shadow-torn\)\)\s*;?[^{}]*\}/);
    for (const { path, css } of stylesheets) {
      expect(css, path).not.toMatch(/box-shadow|text-shadow/);
    }

    // Nor does a component reach for the token through a class.
    const sources = readdirSync(root, { recursive: true, encoding: 'utf8' })
      .map((path) => path.replaceAll('\\', '/'))
      .filter((path) => /\.tsx?$/.test(path) && !/\.test\.tsx?$/.test(path));
    for (const path of sources) {
      expect(readFileSync(join(root, path), 'utf8'), path).not.toMatch(/(?<![\w-])(?:drop-)?shadow-[\w[]/);
    }
  });
});

describe('DraftSlip in the Ask hand-off', () => {
  it('DraftSlip_HandoffMode_NoConfirmReviewLinkAndDiscard', async () => {
    const user = userEvent.setup();
    const onDiscard = vi.fn();
    const onEdit = vi.fn(() => Promise.resolve());
    const draft: DraftTask = { ...warehouse, id: 't4', ref: 'TSK-2034', fromAnswerRef: 'ANS-2209' };
    // No ExcerptProvider: the hand-off slip links to no excerpt.
    render(<DraftSlip mode="handoff" draft={draft} members={members} onEdit={onEdit} boardHref="/board" onDiscard={onDiscard} />);

    expect(screen.getByText('Draft')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: warehouse.title })).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Owner' })).toHaveAttribute('data-warn');
    expect(screen.getByText('No due date')).toBeInTheDocument();
    expect(screen.getByText('REF TSK-2034')).toBeInTheDocument();
    expect(screen.getByText('ANS-2209').closest('span[class]')?.parentElement).toHaveTextContent('From answer ANS-2209');
    expect(screen.getByText('Stays a draft until a person confirms it on the Board.')).toBeInTheDocument();
    expect(document.querySelector('.kiwi-perf')).not.toBeNull();

    // Review on the Board is a link, with its arrow; there is no Confirm and no Edit.
    const review = screen.getByRole('link', { name: 'Review on the Board' });
    expect(review).toHaveAttribute('href', '/board');
    expect(review.querySelector('[data-icon="arrow-right"]')).not.toBeNull();
    expect(screen.queryByRole('button', { name: /confirm/i })).toBeNull();
    expect(screen.queryByRole('button', { name: /edit/i })).toBeNull();
    expect(screen.queryByRole('button', { name: /^transcript/i })).toBeNull();

    await user.click(screen.getByRole('button', { name: 'Discard' }));
    expect(onDiscard).toHaveBeenCalledOnce();

    await user.selectOptions(screen.getByRole('combobox', { name: 'Owner' }), 'Arthit K.');
    expect(onEdit).toHaveBeenCalledExactlyOnceWith({ ownerId: 'u3' });
  });
});

describe('DraftsPanel', () => {
  it('DraftsPanel_BandVariant_ShowsCaptionCountAndEmptyRow', () => {
    const { unmount } = renderPanel({ drafts: [quotation, slides, warehouse], variant: 'band' });
    expect(screen.getByRole('complementary', { name: 'AI drafts' })).toHaveAttribute('data-variant', 'band');
    expect(screen.getByRole('heading', { level: 2, name: 'AI drafts' })).toBeInTheDocument();
    expect(screen.getByText('Nothing here takes effect until you confirm it.')).toBeInTheDocument();
    const count = document.querySelector('.kiwi-pc');
    expect(count).toHaveTextContent('3 to confirm');
    expect(count).not.toHaveAttribute('data-zero');
    expect(within(screen.getByRole('list', { name: 'Drafts to confirm' })).getAllByRole('listitem')).toHaveLength(3);
    // The band has no Board counts block.
    expect(screen.queryByRole('heading', { name: 'Board' })).toBeNull();
    expect(screen.queryByText('To do')).toBeNull();
    unmount();

    renderPanel({ drafts: [], variant: 'band' });
    expect(document.querySelector('.kiwi-pc')).toHaveTextContent('All confirmed');
    expect(document.querySelector('.kiwi-pc')).toHaveAttribute('data-zero');
    expect(screen.queryByRole('list', { name: 'Drafts to confirm' })).toBeNull();
    const empty = document.querySelector<HTMLElement>('[data-drafts-empty]');
    expect(empty).not.toBeNull();
    expect(empty).toHaveAttribute('tabindex', '-1');
    expect(within(empty as HTMLElement).getByRole('heading', { name: 'No drafts waiting' })).toBeInTheDocument();
    expect(within(empty as HTMLElement).getByText('Every AI draft has been confirmed.')).toBeInTheDocument();
    // The Board's band sets the kiwi at 132px.
    expect(empty?.querySelector('svg')).toHaveAttribute('width', '132');
  });

  it('DraftsPanel_PanelVariant_ListsConfirmedThenDraftsThenTheBoardCounts', () => {
    renderPanel({ drafts: [quotation], sealed: [sealedFrom(slides)] });
    expect(screen.getByRole('complementary', { name: 'AI drafts' })).toHaveAttribute('data-variant', 'panel');
    expect(screen.queryByText('Nothing here takes effect until you confirm it.')).toBeNull();
    expect(document.querySelector('.kiwi-pc')).toHaveTextContent('1 to confirm');
    const lists = screen.getAllByRole('list').map((list) => list.getAttribute('aria-label')).filter((name) => name !== null);
    expect(lists.slice(0, 2)).toEqual(['Confirmed', 'Drafts to confirm']);
    const board = screen.getByRole('region', { name: 'Board' });
    expect(within(board).getByText('To do').parentElement).toHaveTextContent('To do7');
    expect(within(board).getByText('Doing').parentElement).toHaveTextContent('Doing4');
    expect(within(board).getByText('Done').parentElement).toHaveTextContent('Done12');
  });

  it('names the confirmed list "Confirmed just now" in the band, where the phone sheet shows it', () => {
    renderPanel({ drafts: [quotation], sealed: [sealedFrom(slides)], variant: 'band' });
    expect(screen.getByRole('list', { name: 'Confirmed just now' })).toBeInTheDocument();
  });

  it('offers a draft its own members when it has them, and the panel list to the rest', () => {
    const ops: DraftTask = { ...slides, id: 't4', title: 'Print the pick lists', members: [{ id: 'u9', name: 'Warehouse lead' }] };
    renderPanel({ drafts: [quotation, ops], variant: 'band' });

    const options = (title: string) => draftItem(title).getAllByRole('option').map((option) => option.textContent);
    expect(options(ops.title)).toEqual(['Choose an owner', 'Warehouse lead']);
    expect(options(quotation.title)).toEqual(['Choose an owner', 'Somchai W.', 'Nok', 'Arthit K.']);
  });
});

describe('DraftsBar', () => {
  it('DraftsBar_ShowsTheCountAndReportsWhetherTheSheetIsOpen', async () => {
    const user = userEvent.setup();
    const onPress = vi.fn();
    const { rerender } = render(<DraftsBar count={3} expanded={false} controls="sheet" onPress={onPress} />);
    const bar = screen.getByRole('button', { name: '3 drafts to confirm' });
    expect(bar).toHaveAttribute('aria-haspopup', 'dialog');
    expect(bar).toHaveAttribute('aria-expanded', 'false');
    expect(bar).toHaveAttribute('aria-controls', 'sheet');
    expect(bar).not.toHaveAttribute('data-zero');
    await user.click(bar);
    expect(onPress).toHaveBeenCalledOnce();

    rerender(<DraftsBar count={1} expanded controls="sheet" onPress={onPress} />);
    expect(screen.getByRole('button', { name: '1 draft to confirm' })).toHaveAttribute('aria-expanded', 'true');

    rerender(<DraftsBar count={0} expanded={false} controls="sheet" onPress={onPress} />);
    expect(screen.getByRole('button', { name: 'No drafts waiting' })).toHaveAttribute('data-zero');
  });

  it('DraftsBar_HandsOutItsButtonThroughRef, so the sheet it opens can return focus to it', () => {
    const bar = createRef<HTMLButtonElement>();

    render(<DraftsBar ref={bar} count={2} expanded={false} controls="sheet" onPress={() => undefined} />);

    expect(bar.current).toBe(screen.getByRole('button', { name: '2 drafts to confirm' }));
    bar.current?.focus();
    expect(screen.getByRole('button', { name: '2 drafts to confirm' })).toHaveFocus();
  });
});

/** The English words of every fixed label: what a Thai screen must never show. */
const ENGLISH_FRAGMENTS = [
  ...new Set(
    Object.values(en)
      .flatMap((value) => (typeof value === 'string' ? [value] : [value.one, value.other]))
      .flatMap((value) => value.split(/\{\w+\}/))
      .map((fragment) => fragment.replace(/[·#,…]/g, ' ').trim())
      .filter((fragment) => /[A-Za-z]{3,}/.test(fragment)),
  ),
];

/** Everything a person or a screen reader can read from the markup: text and every accessible name. */
function readable(root: HTMLElement): string {
  const names = [...root.querySelectorAll('[aria-label], [title]')].flatMap((element) => [
    element.getAttribute('aria-label') ?? '',
    element.getAttribute('title') ?? '',
  ]);
  return [root.textContent, ...names].join('\n');
}

describe('under LabelsProvider lang="th"', () => {
  const thaiMembers: Member[] = [
    { id: 'u1', name: 'สมชาย ว.' },
    { id: 'u2', name: 'นก' },
  ];
  const thaiSource: Source = { kind: 'meeting', id: 'm1', label: 'ประชุมประจำสัปดาห์', channelId: 'c1', offsetMs: 1307000, entryIdx: 20 };
  const thaiDraft: DraftTask = { id: 't1', ref: 'TSK-2031', title: 'ส่งใบเสนอราคาให้ลูกค้า', ownerId: 'u1', dueLabel: 'ศ. 16 ต.ค.', source: thaiSource };
  const thaiUnowned: DraftTask = { ...thaiDraft, id: 't2', ref: 'TSK-2032', title: 'จองคิวคลังสินค้าบางนา', ownerId: null, dueLabel: undefined };

  function inThai(ui: React.ReactElement) {
    return render(
      <LabelsProvider lang="th">
        <ExcerptProvider load={() => Promise.reject(new Error('unused'))}>{ui}</ExcerptProvider>
      </LabelsProvider>,
    );
  }

  const noEnglish = (root: HTMLElement) => {
    for (const fragment of ENGLISH_FRAGMENTS) {
      expect(readable(root), fragment).not.toContain(fragment);
    }
  };

  it('the drafts panel reads in Thai, tags its Thai runs and Latin markers, and keeps REF codes literal', () => {
    const { container } = inThai(
      <DraftsPanel
        variant="panel"
        drafts={[thaiDraft, thaiUnowned]}
        sealed={[{ ...sealedFrom(thaiDraft), id: 't0', title: 'ประกาศวันเปิดตัวใหม่', ownerName: 'นก', dueLabel: undefined, confirmedBy: 'พิมพ์ชนก ส.' }]}
        members={thaiMembers}
        counts={{ todo: 7, doing: 4, done: 12 }}
        onConfirm={() => Promise.resolve()}
        onEdit={() => Promise.resolve()}
      />,
    );
    const heading = screen.getByRole('heading', { level: 2 });
    expect(heading).toHaveTextContent('ฉบับร่างจาก AI');
    expect(within(heading).getByText('AI')).toHaveAttribute('lang', 'en');
    expect(screen.getByText(/รอยืนยัน/)).toHaveAttribute('lang', 'th');
    expect(container).toHaveTextContent('2 รอยืนยัน');
    expect(screen.getAllByText('ฉบับร่าง').length).toBeGreaterThanOrEqual(2);
    expect(screen.getByRole('list', { name: 'ฉบับร่างที่รอยืนยัน' })).toBeInTheDocument();
    expect(screen.getByRole('list', { name: 'ยืนยันแล้ว' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: thaiDraft.title })).toBeInTheDocument();
    expect(screen.getByText(thaiDraft.title)).toHaveAttribute('lang', 'th');
    expect(screen.getAllByRole('combobox', { name: 'ผู้รับผิดชอบ' })).toHaveLength(2);
    expect(screen.getAllByRole('option', { name: 'เลือกผู้รับผิดชอบ' })).toHaveLength(2);
    const confirm = screen.getByRole('button', { name: `ยืนยัน: ${thaiUnowned.title}` });
    expect(confirm).toBeDisabled();
    expect(confirm).toHaveAccessibleDescription('เลือกผู้รับผิดชอบก่อน');
    expect(screen.getByRole('button', { name: `แก้ไข: ${thaiDraft.title}` })).toBeInTheDocument();
    expect(screen.getByText('ไม่มีกำหนดส่ง')).toBeInTheDocument();
    expect(screen.getAllByText('จากการประชุม', { selector: 'span[lang="th"]' })).toHaveLength(3);
    expect(screen.getAllByText('REF TSK-2031')[0]).toHaveAttribute('lang', 'en');
    expect(screen.getByText('ยืนยันโดย').closest('.sr-only')).not.toBeNull();
    expect(screen.getByRole('region', { name: 'บอร์ด' })).toHaveTextContent('ต้องทำ');
    expect(screen.getByRole('img', { name: 'ยืนยันแล้ว' })).toHaveTextContent('ยืนยันแล้ว');
    noEnglish(container);
  });

  it('the band and its empty row read in Thai', () => {
    const { container } = inThai(
      <DraftsPanel variant="band" drafts={[]} sealed={[]} members={thaiMembers} counts={{ todo: 0, doing: 0, done: 0 }} onConfirm={() => Promise.resolve()} onEdit={() => Promise.resolve()} />,
    );
    expect(screen.getByText('ไม่มีอะไรมีผลจนกว่าคุณจะยืนยัน')).toHaveAttribute('lang', 'th');
    expect(document.querySelector('.kiwi-pc')).toHaveTextContent('ยืนยันครบแล้ว');
    expect(document.querySelector('.kiwi-pc')).toHaveAttribute('data-zero');
    expect(screen.getByRole('heading', { name: 'ไม่มีฉบับร่างที่รออยู่' })).toBeInTheDocument();
    expect(screen.getByText(/ยืนยันฉบับร่างจาก/).closest('p')).toHaveTextContent('ยืนยันฉบับร่างจาก AI ครบแล้ว');
    noEnglish(container);
  });

  it('the hand-off slip and the drafts bar read in Thai', () => {
    const { container } = inThai(
      <>
        <DraftSlip
          mode="handoff"
          draft={{ ...thaiUnowned, fromAnswerRef: 'ANS-2209' }}
          members={thaiMembers}
          onEdit={() => Promise.resolve()}
          boardHref="/board"
          onDiscard={() => undefined}
        />
        <DraftsBar count={2} expanded={false} controls="sheet" onPress={() => undefined} />
      </>,
    );
    expect(screen.getByText('ยังเป็นฉบับร่างจนกว่าจะมีคนยืนยันบนบอร์ด')).toHaveAttribute('lang', 'th');
    expect(screen.getByRole('link', { name: 'ตรวจดูบนบอร์ด' })).toHaveAttribute('href', '/board');
    expect(screen.getByRole('button', { name: 'ทิ้งฉบับร่าง' })).toBeInTheDocument();
    expect(container).toHaveTextContent('จากคำตอบ ANS-2209');
    expect(screen.getByRole('button', { name: '2 ฉบับร่างรอยืนยัน' })).toBeInTheDocument();
    noEnglish(container);
  });

  it('announces a confirmation in Thai', async () => {
    const user = userEvent.setup();
    stubReducedMotion(true);
    function Thai() {
      const [drafts, setDrafts] = useState([thaiDraft, { ...thaiUnowned, ownerId: 'u2' }]);
      return (
        <DraftsPanel
          variant="panel"
          drafts={drafts}
          sealed={[]}
          members={thaiMembers}
          counts={{ todo: 0, doing: 0, done: 0 }}
          onConfirm={(id) => {
            setDrafts((current) => current.filter((draft) => draft.id !== id));
            return Promise.resolve();
          }}
          onEdit={() => Promise.resolve()}
        />
      );
    }
    inThai(<Thai />);
    await user.click(screen.getByRole('button', { name: `ยืนยัน: ${thaiDraft.title}` }));
    const status = screen.getByRole('status');
    await waitFor(() => {
      expect(status).toHaveTextContent(`ยืนยันแล้ว: ${thaiDraft.title} เหลือฉบับร่าง 1 ฉบับ`);
    });
    expect(within(status).getByText(/ยืนยันแล้ว/)).toHaveAttribute('lang', 'th');
  });
});
