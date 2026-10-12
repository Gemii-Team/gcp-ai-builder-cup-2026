import { createI18n, type I18nKey } from '@kiwi/core';
import { screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createApi } from '../../api/client';
import { hardNavigate } from '../../navigation';
import { useUiStore } from '../../state/ui';
import { openChannel } from '../../testing/openChannel';
import { createStubApi } from '../../testing/stubApi';

// The contract between the server and the web for every way Start meeting fails. The answers are the files the Go test
// `TestContract_StartMeetingFailures_MatchTheFixturesTheWebReads` (backend/internal/app) compares with the bytes the composed
// server writes; this test feeds each of them through the real connect-web transport (only `fetch` is stubbed), the real
// generated MeetingService client and the real `useStartMeeting` hook on the channel screen, and asserts what the person sees.
// The two sides share only those files: nothing of a body is written out by hand here.

type Fixture = { httpStatus: number; contentType: string; body: unknown };

const files = import.meta.glob<string>('../../../../../packages/api-client/testdata/connect-errors/*.json', { query: '?raw', import: 'default', eager: true });
const fixtures = Object.entries(files).map(([path, text]) => ({
  name: path.slice(path.lastIndexOf('/') + 1).replace(/\.json$/, ''),
  fixture: JSON.parse(text) as Fixture,
}));

/** What the person sees: the page leaves for the Meet consent flow, or one alert says this (a message key). */
type Outcome = 'consent' | I18nKey;

/** The expectation of every fixture. A fixture that is not here fails its own test, so a new answer cannot go unjudged. */
const OUTCOME: Record<string, Outcome> = {
  consent_required: 'consent',
  meet_access_access_denied: 'startMeetingMeetAccess',
  meet_unavailable_api_disabled: 'startMeetingApiDisabled',
  meet_unavailable_rate_limited: 'startMeetingRateLimited',
  meet_unavailable_unavailable: 'startMeetingMeetUnavailable',
  meet_unavailable_refused: 'startMeetingRefused',
  // Anything the web has no specific message for says so generically: never nothing, never a trip to the consent flow.
  internal: 'startMeetingFailed',
  not_found: 'startMeetingFailed',
  permission_denied: 'startMeetingFailed',
  unknown_channel: 'startMeetingFailed',
  // A call answered Unauthenticated is not a query, so the session guard (which watches the query cache) does not send the person to sign-in.
  // What the app does today is the generic alert, and this pins it.
  unauthenticated: 'startMeetingFailed',
};

const en = createI18n('en');

beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

/** The channel screen whose Start meeting goes through the real client to a `fetch` that answers with `fixture`. */
function openWithAnswer({ httpStatus, contentType, body }: Fixture) {
  const fetchStub = vi.fn<typeof fetch>(() => Promise.resolve(new Response(JSON.stringify(body), { status: httpStatus, headers: { 'Content-Type': contentType } })));
  vi.stubGlobal('fetch', fetchStub);
  vi.spyOn(hardNavigate, 'to').mockImplementation(() => undefined);
  vi.spyOn(hardNavigate, 'open').mockImplementation(() => undefined);
  const real = createApi();
  const api = createStubApi();
  api.meetings.startMeeting.mockImplementation((request, options) => real.meetings.startMeeting(request, options));
  return { ...openChannel({}, api), fetchStub };
}

describe('Start meeting: the answers the server writes', () => {
  it('has at least the answers the server can fail with', () => {
    expect(fixtures.map(({ name }) => name)).toEqual(expect.arrayContaining(['consent_required', 'internal', 'unauthenticated']));
  });

  it('has an expectation for every fixture, and a fixture for every expectation', () => {
    const names = fixtures.map(({ name }) => name);
    expect(names.filter((name) => !Object.hasOwn(OUTCOME, name)), 'fixtures with no expectation in OUTCOME').toEqual([]);
    expect(Object.keys(OUTCOME).filter((name) => !names.includes(name)), 'expectations with no fixture').toEqual([]);
  });

  for (const { name, fixture } of fixtures) {
    it(`${name}: ${Object.hasOwn(OUTCOME, name) ? String(OUTCOME[name]) : 'no expectation'}`, async () => {
      if (!Object.hasOwn(OUTCOME, name)) {
        throw new Error(`the fixture ${name} has no expectation in OUTCOME: say what the person must see for it`);
      }
      const outcome = OUTCOME[name];
      const { user, fetchStub } = openWithAnswer(fixture);

      await user.click(await screen.findByRole('button', { name: 'Start meeting' }));

      await waitFor(() => {
        expect(fetchStub).toHaveBeenCalledTimes(1);
      });
      const [target] = fetchStub.mock.calls[0] ?? [];
      expect(typeof target === 'string' ? target : '').toMatch(/\/kiwi\.meetings\.v1\.MeetingService\/StartMeeting$/);
      expect(hardNavigate.open).not.toHaveBeenCalled();
      if (outcome === 'consent') {
        await waitFor(() => {
          expect(hardNavigate.to).toHaveBeenCalledWith('/api/auth/meet-consent?return_to=%2Fc%2Fch-q4');
        });
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
        return;
      }
      const alert = await screen.findByRole('alert');
      expect(alert).toHaveTextContent(en.t(outcome));
      expect(screen.getAllByRole('alert')).toHaveLength(1);
      expect(hardNavigate.to).not.toHaveBeenCalled();
    });
  }
});
