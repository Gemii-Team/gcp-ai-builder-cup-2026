import type { ReactNode, SVGProps } from 'react';
import { cx } from './cx';

export type IconName =
  | 'ask'
  | 'video'
  | 'board'
  | 'chat'
  | 'user'
  | 'file'
  | 'warning'
  | 'clock'
  | 'send'
  | 'back'
  | 'close'
  | 'chevron-up'
  | 'chevron-down'
  | 'paperclip'
  | 'signout'
  | 'check'
  | 'filter'
  | 'search'
  | 'search-none'
  | 'circle'
  | 'arrow-right'
  | 'task-add'
  | 'pen';

// The prototype's symbol set (24-unit grid), drawn as line art in currentColor.
const DRAWINGS: Record<IconName, ReactNode> = {
  ask: (
    <path d="M5 5h14a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2h-7l-4 3v-3H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2zM10 9.5a2 2 0 1 1 3 1.7c-.7.4-1 .8-1 1.5M12 14.5v.01" />
  ),
  video: (
    <>
      <rect x="3" y="6" width="12" height="12" rx="2" />
      <path d="M15 10l6-3v10l-6-3z" />
    </>
  ),
  board: (
    <>
      <rect x="3" y="4" width="18" height="16" rx="2" />
      <path d="M9 4v16M15 4v10" />
    </>
  ),
  chat: <path d="M5 5h14a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2h-7l-4 3v-3H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2zM8 10h8M8 13h5" />,
  user: (
    <>
      <circle cx="12" cy="8" r="4" />
      <path d="M4 21c1-4 4-6 8-6s7 2 8 6" />
    </>
  ),
  file: <path d="M6 3h8l5 5v13H6zM14 3v5h5" />,
  warning: <path d="M12 4l9 16H3zM12 10v4M12 17v.01" />,
  clock: (
    <>
      <circle cx="12" cy="12" r="8" />
      <path d="M12 8v4l3 2" />
    </>
  ),
  send: <path d="M4 12l16-8-6 16-3-7z" />,
  back: <path d="M15 5l-7 7 7 7" />,
  close: <path d="M6 6l12 12M18 6L6 18" />,
  'chevron-up': <path d="M6 15l6-6 6 6" />,
  'chevron-down': <path d="M6 9l6 6 6-6" />,
  paperclip: <path d="M20 11l-8 8a5 5 0 0 1-7-7l8-8a3.5 3.5 0 0 1 5 5l-8 8a2 2 0 0 1-3-3l7-7" />,
  signout: <path d="M9 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h3M15 8l4 4-4 4M19 12H9" />,
  check: <path d="M5 12.5l4.5 4.5L19 7.5" />,
  filter: <path d="M4 7h16M7 12h10M10 17h4" />,
  search: (
    <>
      <circle cx="11" cy="11" r="7" />
      <path d="M16.5 16.5L21 21" />
    </>
  ),
  'search-none': (
    <>
      <circle cx="11" cy="11" r="7" />
      <path d="M8 11h6M16.5 16.5L21 21" />
    </>
  ),
  circle: <circle cx="12" cy="12" r="7" />,
  'arrow-right': <path d="M5 12h14M13 6l6 6-6 6" />,
  'task-add': (
    <>
      <rect x="4" y="4" width="16" height="16" rx="3" />
      <path d="M12 9v6M9 12h6" />
    </>
  ),
  pen: <path d="M4 20l1.2-4.3L16.5 4.8a1.6 1.6 0 0 1 2.3 0l.4.4a1.6 1.6 0 0 1 0 2.3L7.9 18.8zM14.5 6.8l2.7 2.7" />,
};

type IconProps = Omit<SVGProps<SVGSVGElement>, 'children' | 'width' | 'height' | 'viewBox'> & {
  name: IconName;
  /** Pixels; 18 everywhere except where DESIGN.md names another size (16 inline, 20 in the tab bar). */
  size?: number;
};

/** A decorative line icon: 1.75 stroke, round caps, `currentColor`. The control that holds it carries the name. */
export function Icon({ name, size = 18, className, ...rest }: IconProps) {
  return (
    <svg
      viewBox="0 0 24 24"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.75}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
      data-icon={name}
      className={cx('flex-none', className)}
      {...rest}
    >
      {DRAWINGS[name]}
    </svg>
  );
}
