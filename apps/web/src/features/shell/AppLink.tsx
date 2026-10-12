import { useRouter, useRouterState } from '@tanstack/react-router';
import { ButtonLink } from '@kiwi/ui';
import type { ComponentProps, ComponentPropsWithoutRef, MouseEvent } from 'react';

/** The click of a link to a route of this app: a plain click navigates without reloading, a modified one opens the link as usual. */
function useSpaClick(to: string, target: string | undefined, onClick: ((event: MouseEvent<HTMLAnchorElement>) => void) | undefined) {
  const router = useRouter();
  return (event: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(event);
    const modified = event.metaKey || event.ctrlKey || event.shiftKey || event.altKey;
    if (event.defaultPrevented || event.button !== 0 || modified || target === '_blank') {
      return;
    }
    event.preventDefault();
    router.history.push(to);
  };
}

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
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const base = matches ?? to.split(/[?#]/)[0];
  const current = pathname === base || pathname.startsWith(`${base}/`);
  const navigate = useSpaClick(to, rest.target, onClick);
  return <a {...rest} href={to} aria-current={current ? 'page' : undefined} onClick={navigate} />;
}

type AppButtonLinkProps = Omit<ComponentProps<typeof ButtonLink>, 'href'> & { to: string };

/** A `ButtonLink` to a route of this app (the header's Ask and Board): it looks like a button and navigates like `AppLink`. */
export function AppButtonLink({ to, onClick, ...rest }: AppButtonLinkProps) {
  const navigate = useSpaClick(to, rest.target, onClick);
  return <ButtonLink {...rest} href={to} onClick={navigate} />;
}
