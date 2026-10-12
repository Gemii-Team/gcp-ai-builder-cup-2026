import { usePrefersReducedMotion } from '@kiwi/ui';
import { Component, type ReactNode, type RefObject } from 'react';

// DESIGN.md, Moving a card: moved and confirmed cards glide 400ms (expo) from where they were to where they are.
const GLIDE_MS = 400;
/** The theme's `--ease-expo`: the Web Animations API cannot read a custom property. */
const EXPO = 'cubic-bezier(.16,1,.3,1)';
/** Less than this is not a move (sub-pixel layout noise). */
const THRESHOLD = 0.5;

type Spot = { x: number; y: number };

/** Where every card, and every place a card can come from, is: relative to the scroller's own scroll. */
type Spots = Map<string, Spot>;

type GlideProps = {
  /** The scroller that holds the cards. */
  root: RefObject<HTMLElement | null>;
  /** The cell (a string) each card sits in, by task id. */
  cells: ReadonlyMap<string, string>;
  children: ReactNode;
};

type CoreProps = GlideProps & { enabled: boolean };

function spotOf(element: HTMLElement, scroller: HTMLElement, base: DOMRect): Spot {
  const rect = element.getBoundingClientRect();
  return { x: rect.left - base.left + scroller.scrollLeft, y: rect.top - base.top + scroller.scrollTop };
}

function measure(scroller: HTMLElement): Spots {
  const base = scroller.getBoundingClientRect();
  const spots: Spots = new Map();
  for (const element of scroller.querySelectorAll<HTMLElement>('[data-task-id][data-glide], [data-task-id][data-flip]')) {
    spots.set(element.dataset.taskId ?? '', spotOf(element, scroller, base));
  }
  return spots;
}

/**
 * The measuring half of the glide. It is a class because only `getSnapshotBeforeUpdate` runs after React has decided what
 * the DOM becomes and before it changes it, so the "before" rects are always the layout the person was looking at, however
 * it got there (an excerpt that opened inside a card, a lane's meta that arrived, a resize), and nothing is measured at
 * all on a render that moves no card.
 */
class GlideCore extends Component<CoreProps, unknown, Spots | null> {
  private readonly running = new Map<HTMLElement, Animation>();

  getSnapshotBeforeUpdate(previous: CoreProps): Spots | null {
    const scroller = this.props.root.current;
    if (scroller === null || !this.props.enabled || typeof Element.prototype.animate !== 'function') {
      return null;
    }
    // A card moves when it is in another cell than it was, or when it is new here but was on screen already (a draft in
    // the band that has just been confirmed). A filter, a form opening or the server echoing a move change neither.
    const changed = [...this.props.cells].some(([id, cell]) => {
      const was = previous.cells.get(id);
      return was === undefined ? scroller.querySelector(`[data-task-id="${CSS.escape(id)}"]`) !== null : was !== cell;
    });
    if (!changed) {
      return null;
    }
    // A glide still running ends here, so the cards are measured where their layout puts them.
    this.cancelAll();
    return measure(scroller);
  }

  componentDidUpdate(_previous: CoreProps, _state: unknown, before: Spots | null) {
    const scroller = this.props.root.current;
    if (before === null || scroller === null) {
      return;
    }
    const base = scroller.getBoundingClientRect();
    for (const element of scroller.querySelectorAll<HTMLElement>('[data-task-id][data-glide]')) {
      const from = before.get(element.dataset.taskId ?? '');
      if (from === undefined) {
        continue;
      }
      const spot = spotOf(element, scroller, base);
      const dx = from.x - spot.x;
      const dy = from.y - spot.y;
      if (Math.abs(dx) < THRESHOLD && Math.abs(dy) < THRESHOLD) {
        continue;
      }
      const animation = element.animate([{ transform: `translate(${dx}px, ${dy}px)` }, { transform: 'none' }], { duration: GLIDE_MS, easing: EXPO });
      this.running.set(element, animation);
      const done = () => {
        if (this.running.get(element) === animation) {
          this.running.delete(element);
        }
      };
      animation.onfinish = done;
      animation.oncancel = done;
    }
  }

  componentWillUnmount() {
    this.cancelAll();
  }

  private cancelAll() {
    for (const animation of this.running.values()) {
      animation.cancel();
    }
    this.running.clear();
  }

  render() {
    return this.props.children;
  }
}

/**
 * FLIP for the Board's cards. Every card in `root` carries `data-task-id` and `data-glide`; any other element with
 * `data-task-id` and `data-flip` (a draft in the drafts band) is a place a card can come from. When a render moves a card
 * to another cell, each card that moved glides from its old place to its new one (`translate` to `none`, 400ms, expo).
 *
 * Nothing is measured, and nothing animates, on a render that changes no card's cell, under `prefers-reduced-motion`,
 * without the Web Animations API, or (as in jsdom) where layout puts every card at zero. A glide still running when a later
 * move supersedes it, or when the screen goes away, is cancelled.
 */
export function Glide({ root, cells, children }: GlideProps) {
  const reduced = usePrefersReducedMotion();
  return (
    <GlideCore root={root} cells={cells} enabled={!reduced}>
      {children}
    </GlideCore>
  );
}
