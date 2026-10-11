// Renders screens the way the app mounts them (query cache, API clients, language, router and, with `shell`, the signed-in
// shell on a fake realtime client), against a stubbed API. Test support: nothing in the app imports this file.
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { render } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ReactNode } from 'react';
import { ApiProvider } from '../api/client';
import { AppShell } from '../features/shell/AppShell';
import { AppLanguage } from '../i18n';
import { FakeRealtimeClient } from './fakeRealtime';
import { createStubApi, seedApi, type StubApi } from './stubApi';

export function createTestQueryClient(): QueryClient {
  // No retries: a failing request is the test's answer. No garbage collection: no timer outlives the test.
  return new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity }, mutations: { retry: false } } });
}

export type TestRoute = { path: string; element: ReactNode };

type RenderOptions = {
  /** Where the memory history starts. */
  path?: string;
  /** The screens under test, as routes: `{ path: '/c/$channelId', element: <ChannelScreen .../> }`. */
  routes?: TestRoute[];
  /** Whether the routes sit in the signed-in shell (rail, tab bar, realtime). */
  shell?: boolean;
  api?: StubApi;
  client?: FakeRealtimeClient;
  queryClient?: QueryClient;
};

/** The places a link of the rail or the tab bar leads: present so a click lands somewhere. */
const OTHER_SCREENS: TestRoute[] = [
  { path: '/board', element: <p>Board screen</p> },
  { path: '/ask', element: <p>Ask screen</p> },
];

export function renderApp({
  path = '/',
  routes = [],
  shell = true,
  api = seedApi(createStubApi()),
  client = new FakeRealtimeClient(),
  queryClient = createTestQueryClient(),
}: RenderOptions = {}) {
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <ApiProvider api={api}>
          <AppLanguage>
            <Outlet />
          </AppLanguage>
        </ApiProvider>
      </QueryClientProvider>
    ),
  });
  const layout = createRoute({
    getParentRoute: () => rootRoute,
    id: 'app',
    component: () => (shell ? <AppShell client={client} /> : <Outlet />),
  });
  const screens = [...routes, ...OTHER_SCREENS].map(({ path: screenPath, element }) =>
    createRoute({ getParentRoute: () => layout, path: screenPath, component: () => element }),
  );
  const router = createRouter({
    routeTree: rootRoute.addChildren([layout.addChildren(screens)]),
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  const user = userEvent.setup();
  render(<RouterProvider router={router} />);
  return { api, client, queryClient, router, user };
}
