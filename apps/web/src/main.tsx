import { RouterProvider } from '@tanstack/react-router';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { registerSW } from 'virtual:pwa-register';
import { AppProviders } from './AppProviders';
import { router } from './router';
import './styles.css';

registerSW({ immediate: true });

const container = document.getElementById('root');
if (!container) {
  throw new Error('index.html has no #root element');
}

createRoot(container).render(
  <StrictMode>
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>
  </StrictMode>,
);
