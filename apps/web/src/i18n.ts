import { createI18n, type I18n, type I18nKey, type I18nParams } from '@kiwi/core';
import { LabelsProvider, Rich } from '@kiwi/ui';
import { createContext, createElement, useContext, useMemo, type ReactElement, type ReactNode } from 'react';
import { useUiStore } from './state/ui';

// A `.ts` file on purpose: it exports a provider, a hook and a component together, which the fast-refresh lint rule
// only accepts from a file that holds no JSX.

const I18nContext = createContext<I18n>(createI18n('en'));

/**
 * One language for the whole screen: the screen strings of `@kiwi/core` and the component strings of `@kiwi/ui` both
 * follow the interface language the person chose (English until they pick Thai).
 */
export function AppLanguage({ children }: { children: ReactNode }): ReactElement {
  const lang = useUiStore((state) => state.lang);
  const i18n = useMemo(() => createI18n(lang), [lang]);
  return createElement(I18nContext, { value: i18n }, createElement(LabelsProvider, { lang, children }));
}

/** The screen strings in the current language: `t` for markup (`<en>` markers) and `plain` for attributes. */
export function useI18n(): I18n {
  return useContext(I18nContext);
}

/** A screen string as text, its Latin words inside Thai prose tagged `lang="en"`. */
export function T({ k, params }: { k: I18nKey; params?: I18nParams }): ReactElement {
  return createElement(Rich, { text: useI18n().t(k, params) });
}
