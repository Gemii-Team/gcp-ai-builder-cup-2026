// The real route tree (`createAppRouter`) on a memory history against a stubbed API: for the tests of what the tree itself
// decides, the session guard and the sign-in route. A test file that renders the signed-in shell through it mocks
// `realtime/client` so no socket opens. Test support: nothing in the app imports this file.
import type { QueryClient } from '@tanstack/react-query';
import { createMemoryHistory, RouterProvider } from '@tanstack/react-router';
import { render } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AppProviders } from '../AppProviders';
import { createAppRouter } from '../router';
import { createTestQueryClient } from './render';
import { createStubApi, type StubApi } from './stubApi';

export function renderRoutes(path: string, api: StubApi = createStubApi(), queryClient: QueryClient = createTestQueryClient()) {
  const router = createAppRouter({ queryClient, api }, createMemoryHistory({ initialEntries: [path] }));
  const user = userEvent.setup();
  render(
    <AppProviders queryClient={queryClient} api={api}>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  return { api, queryClient, router, user };
}
