import {
  createContext,
  useContext,
  useEffect,
  useLayoutEffect,
  useRef,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
} from 'react';
import { createPortal } from 'react-dom';
import { cx } from './cx';
import { Icon, type IconName } from './icons';

const ROWS = '[role="menuitem"], [role="menuitemradio"]';
const MIN_WIDTH = 184;
const GAP = 4;
const MARGIN = 8;

const CloseMenu = createContext<(() => void) | null>(null);

const ROW =
  'flex w-full min-h-10 items-center gap-3 rounded-md border-0 bg-transparent px-3 text-left font-medium text-ledger-ink cursor-pointer transition-colors duration-150 hover:bg-citation-wash focus-visible:bg-citation-wash focus-visible:outline-offset-[-2px] max-lg:min-h-11';

function rowsOf(menu: HTMLElement | null): HTMLElement[] {
  return menu === null ? [] : [...menu.querySelectorAll<HTMLElement>(ROWS)];
}

/** Fixes the menu to its opener: at least as wide as the opener, on the screen, below or above with the other as fallback. */
function place(menu: HTMLElement, anchor: HTMLElement, placement: 'above' | 'below') {
  const opener = anchor.getBoundingClientRect();
  menu.style.minWidth = `${Math.max(MIN_WIDTH, opener.width)}px`;
  const { width, height } = menu.getBoundingClientRect();
  const below = opener.bottom + GAP;
  const above = opener.top - GAP - height;
  const fitsBelow = below + height <= window.innerHeight - MARGIN;
  const fitsAbove = above >= MARGIN;
  const flip = placement === 'above' ? !fitsAbove && fitsBelow : !fitsBelow && fitsAbove;
  const useAbove = (placement === 'above') !== flip;
  menu.style.left = `${Math.max(MARGIN, Math.min(opener.left, window.innerWidth - width - MARGIN))}px`;
  menu.style.top = `${useAbove ? Math.max(MARGIN, above) : below}px`;
}

type MenuProps = {
  /** Names the menu for assistive tech. */
  label: string;
  open: boolean;
  onClose: () => void;
  /** The control that opened the menu: the menu sits beside it and Escape returns focus to it. */
  anchorRef: RefObject<HTMLElement | null>;
  placement?: 'above' | 'below';
  children: ReactNode;
};

/**
 * A bordered Slip Paper menu, never shadowed. It focuses its first row (the checked one in a radio menu) on open;
 * the arrow keys, Home and End move between rows, Escape closes it and returns focus to the opener, and Tab, a press
 * outside, a scroll or a resize close it. It is placed once, from its opener, in a portal so no transformed or
 * filtered ancestor can displace it.
 */
export function Menu({ label, open, onClose, anchorRef, placement = 'below', children }: MenuProps) {
  const menuRef = useRef<HTMLDivElement>(null);
  const onCloseRef = useRef(onClose);

  useEffect(() => {
    onCloseRef.current = onClose;
  });

  useLayoutEffect(() => {
    if (open && menuRef.current !== null && anchorRef.current !== null) {
      place(menuRef.current, anchorRef.current, placement);
    }
  }, [open, placement, anchorRef]);

  useEffect(() => {
    if (open) {
      const rows = rowsOf(menuRef.current);
      (rows.find((row) => row.getAttribute('aria-checked') === 'true') ?? rows[0])?.focus({ preventScroll: true });
    }
  }, [open]);

  useEffect(() => {
    if (!open) {
      return;
    }
    const close = () => {
      onCloseRef.current();
    };
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (target instanceof Node && !menuRef.current?.contains(target) && !anchorRef.current?.contains(target)) {
        close();
      }
    };
    document.addEventListener('pointerdown', onPointerDown);
    document.addEventListener('scroll', close, true);
    window.addEventListener('resize', close);
    return () => {
      document.removeEventListener('pointerdown', onPointerDown);
      document.removeEventListener('scroll', close, true);
      window.removeEventListener('resize', close);
    };
  }, [open, anchorRef]);

  function onKeyDown(event: KeyboardEvent<HTMLDivElement>) {
    const rows = rowsOf(menuRef.current);
    const current = rows.findIndex((row) => row === document.activeElement);
    const last = rows.length - 1;
    switch (event.key) {
      case 'ArrowDown':
        event.preventDefault();
        rows[current === last ? 0 : current + 1]?.focus();
        break;
      case 'ArrowUp':
        event.preventDefault();
        rows[current <= 0 ? last : current - 1]?.focus();
        break;
      case 'Home':
        event.preventDefault();
        rows[0]?.focus();
        break;
      case 'End':
        event.preventDefault();
        rows[last]?.focus();
        break;
      case 'Escape':
        event.preventDefault();
        event.stopPropagation();
        onClose();
        anchorRef.current?.focus();
        break;
      case 'Tab':
        onClose();
        break;
    }
  }

  if (!open) {
    return null;
  }
  return createPortal(
    <CloseMenu value={onClose}>
      <div
        ref={menuRef}
        role="menu"
        aria-label={label}
        onKeyDown={onKeyDown}
        className="fixed z-40 min-w-[184px] rounded-lg border border-control-line bg-slip-paper p-1"
      >
        {children}
      </div>
    </CloseMenu>,
    document.body,
  );
}

function useCloseMenu(): () => void {
  const close = useContext(CloseMenu);
  if (close === null) {
    throw new Error('A menu row must be rendered inside a <Menu>.');
  }
  return close;
}

type MenuItemProps = {
  icon?: IconName;
  children: ReactNode;
} & ({ href: string; onSelect?: () => void } | { href?: undefined; onSelect: () => void });

/** A row that leads somewhere (`href`, a link) or does something (`onSelect`, a button). Selecting it closes the menu. */
export function MenuItem({ icon, href, onSelect, children }: MenuItemProps) {
  const close = useCloseMenu();
  const select = () => {
    onSelect?.();
    close();
  };
  const content = (
    <>
      {icon !== undefined && <Icon name={icon} className="text-primary" />}
      {children}
    </>
  );
  return href === undefined ? (
    <button type="button" role="menuitem" tabIndex={-1} className={ROW} onClick={select}>
      {content}
    </button>
  ) : (
    <a role="menuitem" tabIndex={-1} href={href} className={ROW} onClick={select}>
      {content}
    </a>
  );
}

type MenuRadioItemProps = {
  checked: boolean;
  onSelect: () => void;
  /** A background token class for the 8px status swatch before the label, such as `bg-doing-teal`. */
  swatch?: string;
  children: ReactNode;
};

/** One choice of a radio menu: the current one is set at 600 with an Ink Teal check at the right. */
export function MenuRadioItem({ checked, onSelect, swatch, children }: MenuRadioItemProps) {
  const close = useCloseMenu();
  return (
    <button
      type="button"
      role="menuitemradio"
      aria-checked={checked}
      tabIndex={-1}
      className={cx(ROW, checked && 'font-semibold')}
      onClick={() => {
        onSelect();
        close();
      }}
    >
      {swatch !== undefined && <i aria-hidden="true" className={cx('size-2 flex-none rounded-hair', swatch)} />}
      {children}
      {checked && <Icon name="check" className="ml-auto text-primary" />}
    </button>
  );
}
