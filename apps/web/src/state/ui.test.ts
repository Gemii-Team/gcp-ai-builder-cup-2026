import { beforeEach, describe, expect, it } from 'vitest';
import { UI_STORAGE_KEY, useUiStore } from './ui';

beforeEach(() => {
  localStorage.clear();
  useUiStore.setState({ lang: 'en', readSeq: {}, lastChannelId: null, draftsSheetOpen: false });
  document.documentElement.lang = 'en';
});

describe('the language', () => {
  it('is English until the person picks Thai', () => {
    expect(useUiStore.getState().lang).toBe('en');
  });

  it('sets the document language, and sets it back', () => {
    useUiStore.getState().setLang('th');
    expect(document.documentElement.lang).toBe('th');

    useUiStore.getState().setLang('en');
    expect(document.documentElement.lang).toBe('en');
  });

  it('is kept in localStorage, with how far each channel was read, and nothing else', () => {
    useUiStore.getState().setLang('th');
    useUiStore.getState().markRead('ch-q4', 7);
    useUiStore.getState().setDraftsSheetOpen(true);

    const stored = JSON.parse(localStorage.getItem(UI_STORAGE_KEY) ?? '{}') as { state: Record<string, unknown> };

    expect(stored.state).toEqual({ lang: 'th', readSeq: { 'ch-q4': 7 }, lastChannelId: null });
  });
});

describe('read markers', () => {
  it('start at zero for an empty channel, so its first message counts as unread', () => {
    useUiStore.getState().markRead('ch-new', 0);

    expect(useUiStore.getState().readSeq['ch-new']).toBe(0);
  });

  it('only move forward', () => {
    useUiStore.getState().markRead('ch-q4', 9);
    useUiStore.getState().markRead('ch-q4', 4);

    expect(useUiStore.getState().readSeq['ch-q4']).toBe(9);
  });
});
