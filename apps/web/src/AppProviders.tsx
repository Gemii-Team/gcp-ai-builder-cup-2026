import { QueryClientProvider, type QueryClient } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { ApiProvider, type Api } from './api/client';
import { AppLanguage } from './i18n';

/**
 * What every screen stands on: the query cache, the API clients (`/api` on the page's own origin) and the interface
 * language, which the screen strings and the `@kiwi/ui` strings both follow. The cache and the clients are made once, in
 * `main.tsx`, because the router's guards ask the server who is signed in through the same ones.
 */
export function AppProviders({ queryClient, api, children }: { queryClient: QueryClient; api: Api; children: ReactNode }) {
  return (
    <QueryClientProvider client={queryClient}>
      <ApiProvider api={api}>
        <AppLanguage>{children}</AppLanguage>
      </ApiProvider>
    </QueryClientProvider>
  );
}
