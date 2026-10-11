import { RouterProvider } from '@tanstack/react-router';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { registerSW } from 'virtual:pwa-register';
import { AppProviders } from './AppProviders';
import { createApi } from './api/client';
import { createQueryClient } from './api/queryClient';
import { createAppRouter } from './router';
import './styles.css';

registerSW({ immediate: true });

const container = document.getElementById('root');
if (!container) {
  throw new Error('index.html has no #root element');
}

// The router's guards read the session through the same cache and clients the screens use, so all three are made here.
const queryClient = createQueryClient();
const api = createApi();
const router = createAppRouter({ queryClient, api });

createRoot(container).render(
  <StrictMode>
    <AppProviders queryClient={queryClient} api={api}>
      <RouterProvider router={router} />
    </AppProviders>
  </StrictMode>,
);
