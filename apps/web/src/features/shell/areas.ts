import type { I18nKey } from '@kiwi/core';
import type { IconName } from '@kiwi/ui';

/** The app areas the thin rail switches between, Asana-style: each opens a contextual inner sidebar. */
export type AreaId = 'chat' | 'work' | 'calendar' | 'agents';

export type Area = { id: AreaId; icon: IconName; label: I18nKey };

/** The areas, top to bottom in the rail. */
export const AREAS: readonly Area[] = [
  { id: 'chat', icon: 'chat', label: 'areaChat' },
  { id: 'work', icon: 'board', label: 'areaWork' },
  { id: 'calendar', icon: 'clock', label: 'areaCalendar' },
  { id: 'agents', icon: 'ask', label: 'areaAgents' },
];

/** The area the current path belongs to; chat is the home area a bare path falls into. */
export function areaOf(pathname: string): AreaId {
  if (pathname.startsWith('/c/')) {
    return 'chat';
  }
  if (pathname === '/board' || pathname.startsWith('/meeting')) {
    return 'work';
  }
  if (pathname.startsWith('/calendar')) {
    return 'calendar';
  }
  if (pathname.startsWith('/ask')) {
    return 'agents';
  }
  return 'chat';
}
