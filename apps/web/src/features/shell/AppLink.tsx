import { useRouter, useRouterState } from '@tanstack/react-router';
import type { ComponentPropsWithoutRef, MouseEvent } from 'react';

type AppLinkProps = Omit<ComponentPropsWithoutRef<'a'>, 'href'> & {
  /** The path, with its query, of a route of this app. */
  to: string;
  /** The path that marks the link as the current route: the link's own path by default. */
  matches?: string;
};

/**
 * A link to a route of this app that navigates without reloading, and that reports `aria-current` while the route (the
 * path, not its query) is the current one. It takes a plain path, so the rail and the tab bar can point at routes of the
 * other screens without this screen importing them.
 */
export function AppLink({ to, matches, onClick, ...rest }: AppLinkProps) {
  const router = useRouter();
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const base = matches ?? to.split(/[?#]/)[0];
  const current = pathname === base || pathname.startsWith(`${base}/`);

  function navigate(event: MouseEvent<HTMLAnchorElement>) {
    onClick?.(event);
    const modified = event.metaKey || event.ctrlKey || event.shiftKey || event.altKey;
    if (event.defaultPrevented || event.button !== 0 || modified || rest.target === '_blank') {
      return;
    }
    event.preventDefault();
    router.history.push(to);
  }

  return <a {...rest} href={to} aria-current={current ? 'page' : undefined} onClick={navigate} />;
}
