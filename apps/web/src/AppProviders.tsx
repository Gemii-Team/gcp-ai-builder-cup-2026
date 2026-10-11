import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { useState, type ReactNode } from 'react';
import { ApiProvider, createApi } from './api/client';
import { AppLanguage } from './i18n';

function createQueryClient(): QueryClient {
  // A request that fails twice is not going to succeed by itself: say so after a moment, not after minutes of retries.
  return new QueryClient({ defaultOptions: { queries: { retry: 1, staleTime: 30_000 } } });
}

/**
 * What every screen stands on: the query cache, the API clients (`/api` on the page's own origin) and the interface
 * language, which the screen strings and the `@kiwi/ui` strings both follow.
 */
export function AppProviders({ children }: { children: ReactNode }) {
  const [queryClient] = useState(createQueryClient);
  const [api] = useState(() => createApi());
  return (
    <QueryClientProvider client={queryClient}>
      <ApiProvider api={api}>
        <AppLanguage>{children}</AppLanguage>
      </ApiProvider>
    </QueryClientProvider>
  );
}
