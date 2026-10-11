import { createContext, Fragment, useContext, useMemo, type ReactNode } from 'react';
import { EnMarker, langRuns } from '../LangRuns';
import { Numeral } from '../Numeral';
import { en, type Plural, type UiLabels } from './en';
import { th } from './th';

export type { UiLabels } from './en';

export type UiLang = 'en' | 'th';

export type Labels = {
  lang: UiLang;
  /**
   * A label as plain text, for an `aria-label` or any other attribute: `{name}` filled from `params`, `<en>` markers
   * dropped, the plural form chosen by `params.count`. A placeholder with no value throws.
   */
  text: (key: keyof UiLabels, params?: Record<string, string | number>) => string;
  /**
   * A label as something to render: Thai runs tagged `lang="th"`, `<en>Latin</en>` as `lang="en"`, `{name}` replaced by
   * the node in `params`. A number parameter renders as a mono `Numeral`; `params.count` also picks the plural form.
   */
  node: (key: keyof UiLabels, params?: Record<string, ReactNode>) => ReactNode;
};

const TABLES: Record<UiLang, UiLabels> = { en, th };

// Thai has a single plural category (`other`), so its tables repeat one text for both forms.
const PLURALS: Record<UiLang, Intl.PluralRules> = { en: new Intl.PluralRules('en'), th: new Intl.PluralRules('th') };

// `<en>Latin</en>` (group 1) or `{name}` (group 2).
const TOKEN = /<en>(.*?)<\/en>|\{(\w+)\}/gs;

function wording(lang: UiLang, value: string | Plural, count: unknown): string {
  if (typeof value === 'string') {
    return value;
  }
  return PLURALS[lang].select(typeof count === 'number' ? count : 0) === 'one' ? value.one : value.other;
}

function createLabels(lang: UiLang, overrides?: Partial<UiLabels>): Labels {
  const table: UiLabels = overrides === undefined ? TABLES[lang] : { ...TABLES[lang], ...overrides };

  function value<Param>(key: keyof UiLabels, name: string, params: Record<string, Param>): Param {
    if (!(name in params)) {
      throw new Error(`The label "${key}" needs a value for {${name}}.`);
    }
    return params[name];
  }

  return {
    lang,
    text(key, params = {}) {
      return wording(lang, table[key], params.count).replace(TOKEN, (_, latin: string | undefined, name: string) =>
        latin ?? String(value(key, name, params)),
      );
    },
    node(key, params = {}) {
      const template = wording(lang, table[key], params.count);
      const parts: ReactNode[] = [];
      let last = 0;
      const prose = (end: number) => {
        if (end > last) {
          const run = template.slice(last, end);
          parts.push(<Fragment key={`t${last}`}>{lang === 'th' ? langRuns(run) : run}</Fragment>);
        }
      };
      for (const token of template.matchAll(TOKEN)) {
        prose(token.index);
        if (token[1] === undefined) {
          const param = value(key, token[2], params);
          parts.push(<Fragment key={`p${token.index}`}>{typeof param === 'number' ? <Numeral>{param}</Numeral> : param}</Fragment>);
        } else {
          parts.push(<EnMarker key={`e${token.index}`}>{token[1]}</EnMarker>);
        }
        last = token.index + token[0].length;
      }
      prose(template.length);
      return parts;
    },
  };
}

// Without a provider the components still render, in English.
const LabelsContext = createContext<Labels>(createLabels('en'));

/**
 * Sets the language of every fixed string the `@kiwi/ui` components draw. `labels` overrides single keys; pass a stable
 * object (a module constant or a memo), since a new one rebuilds the table for every consumer.
 */
export function LabelsProvider({
  lang,
  labels,
  children,
}: {
  lang: UiLang;
  labels?: Partial<UiLabels>;
  children: ReactNode;
}) {
  const value = useMemo(() => createLabels(lang, labels), [lang, labels]);
  return <LabelsContext value={value}>{children}</LabelsContext>;
}

export function useLabels(): Labels {
  return useContext(LabelsContext);
}
