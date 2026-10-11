import type { Lang } from '@kiwi/core';
import { create } from 'zustand';
import { createJSONStorage, persist } from 'zustand/middleware';

/**
 * Local UI state, and nothing that belongs to the server: the interface language, how far each channel has been read,
 * the channel last open and whether the phone's drafts sheet is open. Server data lives in the query cache.
 */
export type UiState = {
  /** English is the default; Thai is a choice the person makes, kept in `localStorage`. */
  lang: Lang;
  setLang: (lang: Lang) => void;
  /** The newest seq of each channel the person has seen. The rail's unread pill is the channel's head minus it. */
  readSeq: Record<string, number>;
  markRead: (channelId: string, seq: number) => void;
  /** Where the Chat tab and the start page lead. */
  lastChannelId: string | null;
  setLastChannel: (channelId: string) => void;
  draftsSheetOpen: boolean;
  setDraftsSheetOpen: (open: boolean) => void;
};

/** `lang`, `readSeq` and `lastChannelId` survive a reload; the sheet does not. */
export const UI_STORAGE_KEY = 'kiwi.ui';

export const useUiStore = create<UiState>()(
  persist(
    (set) => ({
      lang: 'en',
      setLang: (lang) => {
        set({ lang });
      },
      readSeq: {},
      markRead: (channelId, seq) => {
        set((state) => {
          const read = state.readSeq[channelId];
          return read === undefined || seq > read ? { readSeq: { ...state.readSeq, [channelId]: seq } } : state;
        });
      },
      lastChannelId: null,
      setLastChannel: (lastChannelId) => {
        set({ lastChannelId });
      },
      draftsSheetOpen: false,
      setDraftsSheetOpen: (draftsSheetOpen) => {
        set({ draftsSheetOpen });
      },
    }),
    {
      name: UI_STORAGE_KEY,
      storage: createJSONStorage(() => localStorage),
      partialize: ({ lang, readSeq, lastChannelId }) => ({ lang, readSeq, lastChannelId }),
    },
  ),
);

/** The document's language follows the interface language, so Thai text and the screen reader's voice agree. */
function applyLanguage(lang: Lang) {
  document.documentElement.lang = lang;
}

applyLanguage(useUiStore.getState().lang);
useUiStore.subscribe((state, previous) => {
  if (state.lang !== previous.lang) {
    applyLanguage(state.lang);
  }
});

/** The interface language and its setter, for the language switch. */
export function useLanguage(): [Lang, (lang: Lang) => void] {
  const lang = useUiStore((state) => state.lang);
  const setLang = useUiStore((state) => state.setLang);
  return [lang, setLang];
}
