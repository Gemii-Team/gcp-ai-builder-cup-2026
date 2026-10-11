import { createI18n, type I18n, type I18nKey, type I18nParams } from '@kiwi/core';
import { LabelsProvider, LangRuns, Rich } from '@kiwi/ui';
import { createContext, createElement, Fragment, useContext, useMemo, type ReactElement, type ReactNode } from 'react';
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

const HOLE = /\{(\w+)\}/g;

/**
 * A screen string whose `{name}` placeholders are filled with elements rather than text (a domain in mono, a workspace
 * name tagged for its language). The text around them goes through `Rich`, so Latin words in Thai prose keep `lang="en"`.
 */
export function TSlots({ k, params = {}, slots }: { k: I18nKey; params?: I18nParams; slots: Record<string, ReactNode> }): ReactElement {
  // Leaving each slot's own `{name}` in the filled text marks where its element goes.
  const holes = Object.fromEntries(Object.keys(slots).map((name) => [name, `{${name}}`]));
  const text = useI18n().t(k, { ...params, ...holes });
  const parts: ReactNode[] = [];
  let last = 0;
  for (const hole of text.matchAll(HOLE)) {
    if (hole.index > last) {
      parts.push(createElement(Rich, { key: `t${String(last)}`, text: text.slice(last, hole.index) }));
    }
    parts.push(createElement(Fragment, { key: `s${String(hole.index)}` }, slots[hole[1]]));
    last = hole.index + hole[0].length;
  }
  if (last < text.length) {
    parts.push(createElement(Rich, { key: `t${String(last)}`, text: text.slice(last) }));
  }
  return createElement(Fragment, null, parts);
}

/**
 * Text a person or an admin wrote (a workspace name) set into a screen string. Thai runs carry `lang="th"`; in the Thai
 * interface a name made of Latin letters carries `lang="en"`, as every Latin word of a Thai sentence does.
 */
export function UserText({ text }: { text: string }): ReactElement {
  const { lang } = useI18n();
  const latinOnly = /[A-Za-z]/.test(text) && !/[\u0E00-\u0E7F]/.test(text);
  return lang === 'th' && latinOnly ? createElement('span', { lang: 'en' }, text) : createElement(LangRuns, { text });
}
