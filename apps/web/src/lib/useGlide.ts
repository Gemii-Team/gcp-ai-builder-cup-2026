import { usePrefersReducedMotion } from '@kiwi/ui';
import { useEffect, useLayoutEffect, useRef, type RefObject } from 'react';

// DESIGN.md, Moving a card: moved and confirmed cards glide 400ms (expo) from where they were to where they are.
const GLIDE_MS = 400;
/** The theme's `--ease-expo`: the Web Animations API cannot read a custom property. */
const EXPO = 'cubic-bezier(.16,1,.3,1)';
/** Less than this is not a move (sub-pixel layout noise). */
const THRESHOLD = 0.5;

type Spot = { x: number; y: number };

type Snapshot = { cells: ReadonlyMap<string, string>; spots: Map<string, Spot> };

/**
 * FLIP for the Board's cards. Every card in `root` carries `data-task-id` and `data-glide`; any other element with
 * `data-task-id` and `data-flip` (a draft in the drafts band) is a place a card can come from. After every commit the
 * hook measures them, relative to `root`'s own scroll, and when a card has changed cell (`cells` maps a task id to the
 * cell it sits in) or has arrived from somewhere else on screen, each card that moved is animated from its old place to
 * its new one. A commit that changes nothing about which cell holds which card (a form opening, a filter) moves nothing.
 *
 * It does nothing without layout (jsdom measures zeros, so no card has moved), without the Web Animations API, or
 * under `prefers-reduced-motion`. Animations still running when the screen goes away are cancelled.
 */
export function useGlide(root: RefObject<HTMLElement | null>, cells: ReadonlyMap<string, string>): void {
  const reduced = usePrefersReducedMotion();
  const last = useRef<Snapshot | null>(null);
  const running = useRef(new Map<HTMLElement, Animation>());

  useLayoutEffect(() => {
    const scroller = root.current;
    if (scroller === null) {
      return;
    }
    const before = last.current;
    const motion = !reduced && typeof Element.prototype.animate === 'function';
    let changed = false;
    if (before !== null && motion) {
      for (const [id, cell] of cells) {
        const was = before.cells.get(id);
        if (was === undefined ? before.spots.has(id) : was !== cell) {
          changed = true;
          break;
        }
      }
    }
    if (changed) {
      // A card that is still gliding is measured where its layout puts it, so the earlier glide ends here.
      for (const animation of running.current.values()) {
        animation.cancel();
      }
      running.current.clear();
    }

    const base = scroller.getBoundingClientRect();
    const spots = new Map<string, Spot>();
    for (const element of scroller.querySelectorAll<HTMLElement>('[data-task-id][data-glide], [data-task-id][data-flip]')) {
      const id = element.dataset.taskId ?? '';
      const rect = element.getBoundingClientRect();
      const spot = { x: rect.left - base.left + scroller.scrollLeft, y: rect.top - base.top + scroller.scrollTop };
      const from = before?.spots.get(id);
      if (!changed && running.current.has(element) && from !== undefined) {
        // Mid-glide the rect includes the glide: keep where its layout put it.
        spots.set(id, from);
        continue;
      }
      spots.set(id, spot);
      if (changed && from !== undefined && element.hasAttribute('data-glide')) {
        const dx = from.x - spot.x;
        const dy = from.y - spot.y;
        if (Math.abs(dx) >= THRESHOLD || Math.abs(dy) >= THRESHOLD) {
          const animation = element.animate([{ transform: `translate(${dx}px, ${dy}px)` }, { transform: 'none' }], { duration: GLIDE_MS, easing: EXPO });
          running.current.set(element, animation);
          const done = () => {
            if (running.current.get(element) === animation) {
              running.current.delete(element);
            }
          };
          animation.onfinish = done;
          animation.oncancel = done;
        }
      }
    }
    last.current = { cells, spots };
  });

  useEffect(() => {
    const animations = running.current;
    return () => {
      for (const animation of animations.values()) {
        animation.cancel();
      }
      animations.clear();
    };
  }, []);
}
