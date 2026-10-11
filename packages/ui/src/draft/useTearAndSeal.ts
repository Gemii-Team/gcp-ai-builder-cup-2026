import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { useLabels, type Labels } from '../labels';
import { usePrefersReducedMotion } from '../slip/Pending';

// DESIGN.md, Confirm: tear and seal. The tear, the glide of what remains and the empty state's fade take 400ms; the seal
// stamps in 120ms late over 280ms.
const TEAR_MS = 400;
const STAMP_MS = 280;
const STAMP_DELAY_MS = 120;
const LIFT_MS = 450;
const TEAR_SLIDE = 'translateY(28px) rotate(3deg)';
/** The theme's `--ease-expo`: the Web Animations API cannot read a custom property. */
const EXPO = 'cubic-bezier(.16,1,.3,1)';
/** A live region only speaks a change, so the text is cleared first and set again a moment later. */
const SAY_DELAY_MS = 40;

/** Everything about the panel that the confirmation must read before the draft leaves it. */
type Before = {
  /** The draft's list item. */
  li: HTMLElement;
  /** The panel (`[data-drafts-root]`) that holds it. */
  root: HTMLElement;
  id: string;
  title: string;
  /** Its place among the drafts, where the focus goes next. */
  index: number;
  /** Whether to animate at all. */
  motion: boolean;
  /** The first rect of every item and the draft's own, for the FLIP glide. */
  rects: Map<Element, DOMRect>;
  liRect: DOMRect;
  /** The stub's clone, made before the slip changes and shown only once the server has agreed. */
  ghost: HTMLElement | null;
};

type Waiter = { li: HTMLElement; resolve: (gone: boolean) => void };

export type TearAndSeal = {
  /**
   * Confirms the draft that `li` holds. `run` makes the server call and is awaited FIRST: if it rejects, `confirm` rejects
   * with it and nothing has changed, torn or spoken. Once it resolves and the list has dropped the draft, the stub tears
   * away as a ghost, the seal stamps in, the drafts that remain glide up, and the result is announced.
   */
  confirm: (li: HTMLElement, run: () => Promise<void>) => Promise<void>;
  /** The text for the panel's live region (`role="status"`). */
  announcement: string;
};

/** The stub's clone, taken out of the accessibility tree and the tab order and pinned where the stub is. */
function makeGhost(stub: Element): HTMLElement | null {
  const rect = stub.getBoundingClientRect();
  const ghost = stub.cloneNode(true);
  if (!(ghost instanceof HTMLElement)) {
    return null;
  }
  ghost.classList.add('kiwi-ghost');
  ghost.setAttribute('aria-hidden', 'true');
  ghost.setAttribute('inert', '');
  ghost.querySelectorAll('[id]').forEach((element) => {
    element.removeAttribute('id');
  });
  Object.assign(ghost.style, { left: `${rect.left}px`, top: `${rect.top}px`, width: `${rect.width}px`, height: `${rect.height}px` });
  return ghost;
}

function take(li: HTMLElement, motion: boolean): Before | null {
  const root = li.closest<HTMLElement>('[data-drafts-root]');
  const list = li.parentElement;
  if (root === null || list === null) {
    return null;
  }
  const stub = li.querySelector('.kiwi-stub');
  return {
    li,
    root,
    id: li.dataset.taskId ?? '',
    title: li.querySelector('.kiwi-d-title')?.textContent?.trim() ?? '',
    index: Array.from(list.children).indexOf(li),
    motion,
    rects: new Map(motion ? Array.from(root.querySelectorAll('[data-flip]'), (item): [Element, DOMRect] => [item, item.getBoundingClientRect()]) : []),
    liRect: li.getBoundingClientRect(),
    ghost: motion && stub !== null ? makeGhost(stub) : null,
  };
}

/** FLIP: what moved is drawn at its old place and glides to its new one. */
function glide(rects: Map<Element, DOMRect>) {
  for (const [element, first] of rects) {
    if (!element.isConnected) {
      continue;
    }
    const last = element.getBoundingClientRect();
    const dx = first.left - last.left;
    const dy = first.top - last.top;
    if (Math.abs(dx) >= 0.5 || Math.abs(dy) >= 0.5) {
      element.animate([{ transform: `translate(${dx}px,${dy}px)` }, { transform: 'none' }], { duration: TEAR_MS, easing: EXPO });
    }
  }
}

/** The next Confirm, or the owner select of a draft that cannot be confirmed yet, or its first live button. */
function focusIn(item: Element | undefined) {
  const target =
    item?.querySelector<HTMLElement>('[data-confirm]:not(:disabled)') ??
    item?.querySelector<HTMLElement>('select') ??
    item?.querySelector<HTMLElement>('button:not(:disabled)');
  target?.focus();
}

/**
 * The confirmation of a draft: the server first, then the tear. See `TearAndSeal.confirm`. It belongs to the component
 * that renders the drafts list, which must render `announcement` in a live region and mark its elements:
 * `[data-drafts-root]` (the panel), `[data-drafts-body]` (its scroller), `[data-drafts-list]` and `[data-sealed-list]`
 * with items carrying `data-flip` and `data-task-id`, and `[data-drafts-empty]` (focusable) for when none are left.
 */
export function useTearAndSeal(): TearAndSeal {
  const labels = useLabels();
  const reduced = usePrefersReducedMotion();
  const [announcement, setAnnouncement] = useState('');
  const waiting = useRef(new Set<Waiter>());
  const timers = useRef(new Set<number>());
  const ghosts = useRef(new Set<HTMLElement>());
  const sayTimer = useRef<number>(undefined);
  // Whether the host is on screen. Unlike the cleanup below, it also covers what is still in flight: the server call
  // that resolves after an unmount must schedule nothing and set no state.
  const mounted = useRef(false);

  useEffect(() => {
    mounted.current = true;
    const owned = { waiting: waiting.current, timers: timers.current, ghosts: ghosts.current };
    return () => {
      mounted.current = false;
      owned.timers.forEach((timer) => {
        window.clearTimeout(timer);
      });
      owned.timers.clear();
      owned.ghosts.forEach((ghost) => {
        ghost.remove();
      });
      owned.ghosts.clear();
      owned.waiting.forEach((waiter) => {
        waiter.resolve(false);
      });
      owned.waiting.clear();
    };
  }, []);

  // The list is the app's to change. Every commit of the panel checks whether a draft being confirmed has left it.
  useLayoutEffect(() => {
    for (const waiter of waiting.current) {
      if (!waiter.li.isConnected) {
        waiting.current.delete(waiter);
        waiter.resolve(true);
      }
    }
  });

  const later = useCallback((run: () => void, ms: number) => {
    const timer = window.setTimeout(() => {
      timers.current.delete(timer);
      run();
    }, ms);
    timers.current.add(timer);
    return timer;
  }, []);

  const removed = useCallback(
    (li: HTMLElement) =>
      new Promise<boolean>((resolve) => {
        if (li.isConnected) {
          waiting.current.add({ li, resolve });
        } else {
          resolve(true);
        }
      }),
    [],
  );

  const play = useCallback(
    (before: Before, text: Labels) => {
      if (!mounted.current) {
        return;
      }
      const { root, id, title, motion, ghost } = before;
      const body = root.querySelector<HTMLElement>('[data-drafts-body]');
      if (body !== null) {
        body.scrollTop = 0;
      }
      const left = root.querySelectorAll('[data-draft]').length;
      const sealed = Array.from(root.querySelectorAll<HTMLElement>('[data-sealed-list] > [data-task-id]')).find((item) => item.dataset.taskId === id);
      const empty = root.querySelector<HTMLElement>('[data-drafts-empty]');

      if (motion) {
        if (sealed !== undefined) {
          before.rects.set(sealed, before.liRect);
        }
        glide(before.rects);
        if (ghost !== null) {
          document.body.append(ghost);
          ghosts.current.add(ghost);
          ghost.animate([{ transform: 'none', opacity: 1 }, { transform: TEAR_SLIDE, opacity: 0 }], {
            duration: TEAR_MS,
            easing: EXPO,
            fill: 'forwards',
          });
          later(() => {
            ghost.remove();
            ghosts.current.delete(ghost);
          }, TEAR_MS);
        }
        if (sealed !== undefined) {
          // The sealed slip glides over its neighbours, so it stays on top until it has settled.
          sealed.style.zIndex = '2';
          later(() => {
            sealed.style.zIndex = '';
          }, LIFT_MS);
          sealed.querySelector('.kiwi-seal')?.animate(
            [
              { opacity: 0, transform: 'rotate(-18deg) scale(1.5)' },
              { opacity: 1, transform: 'rotate(-8deg) scale(1)' },
            ],
            { duration: STAMP_MS, delay: STAMP_DELAY_MS, easing: EXPO, fill: 'backwards' },
          );
        }
        if (left === 0) {
          empty?.animate([{ opacity: 0 }, { opacity: 1 }], { duration: TEAR_MS, easing: EXPO });
        }
      }

      window.clearTimeout(sayTimer.current);
      setAnnouncement('');
      sayTimer.current = later(() => {
        setAnnouncement(left > 0 ? text.text('confirmedAnnouncement', { title, count: left }) : text.text('confirmedAnnouncementLast', { title }));
      }, SAY_DELAY_MS);

      if (left > 0) {
        const list = root.querySelector('[data-drafts-list]');
        focusIn(list?.children[Math.min(before.index, list.children.length - 1)]);
      } else {
        empty?.focus({ preventScroll: true });
      }
    },
    [later],
  );

  const confirm = useCallback(
    async (li: HTMLElement, run: () => Promise<void>) => {
      const before = take(li, !reduced && typeof Element.prototype.animate === 'function');
      // The server decides first. A rejection leaves the slip exactly where it was: nothing has been torn, sealed or said.
      await run();
      // The panel may have gone while the server thought (a route change): there is no slip left to tear, and a ghost
      // appended now would float over whatever screen replaced it.
      if (before === null || !mounted.current) {
        return;
      }
      void removed(li).then((gone) => {
        if (gone) {
          play(before, labels);
        }
      });
    },
    [reduced, removed, play, labels],
  );

  return { confirm, announcement };
}
