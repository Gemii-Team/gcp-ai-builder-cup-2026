/**
 * Leaves the single-page app: a full navigation to a route the server owns (sign-out ends at `/signin`, the Meet consent
 * flow starts at `/api/auth/meet-consent`), or a link opened in a new tab. A seam, so a test can see where the page went.
 */
export const hardNavigate = {
  to: (url: string): void => {
    window.location.assign(url);
  },
  open: (url: string): void => {
    window.open(url, '_blank', 'noopener,noreferrer');
  },
};
