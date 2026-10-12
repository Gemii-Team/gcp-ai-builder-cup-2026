import { en, type I18nTable, type Plural } from './en';
import { th } from './th';

export type Lang = 'en' | 'th';

/** A key of the English table: the source of truth every language fills. */
export type I18nKey = keyof typeof en;

/** Values for the `{name}` placeholders of a string; `count` also picks the plural form. */
export type I18nParams = Record<string, string | number>;

export type I18n = {
  readonly lang: Lang;
  /**
   * A string as markup for `<Rich>`: `{name}` filled from `params`, the plural form chosen by `params.count`, and a
   * Latin word inside Thai prose left marked `<en>Google</en>`. A placeholder with no value throws. An unknown key
   * throws in development and test, and shows the key itself in production.
   */
  t: (key: I18nKey, params?: I18nParams) => string;
  /** The same string with the `<en>` markers removed, for an attribute, `aria-label` or the document title. */
  plain: (key: I18nKey, params?: I18nParams) => string;
};

const TABLES: Record<Lang, I18nTable> = { en, th };

// Thai has a single plural category (`other`), so its tables repeat one text for both forms.
const PLURALS: Record<Lang, Intl.PluralRules> = { en: new Intl.PluralRules('en'), th: new Intl.PluralRules('th') };

const PLACEHOLDER = /\{(\w+)\}/g;
const EN_MARKER = /<\/?en>/g;

export function createI18n(lang: Lang = 'en'): I18n {
  const table: Readonly<Record<string, string | Plural>> = TABLES[lang];
  const plurals = PLURALS[lang];

  function t(key: I18nKey, params: I18nParams = {}): string {
    const entry: string | Plural | undefined = table[key];
    if (entry === undefined) {
      if (import.meta.env.DEV) {
        throw new Error(`The i18n key "${key}" is missing from the ${lang} table.`);
      }
      return key;
    }
    const template = typeof entry === 'string' ? entry : plurals.select(Number(params.count)) === 'one' ? entry.one : entry.other;
    return template.replace(PLACEHOLDER, (_, name: string) => {
      if (!(name in params)) {
        throw new Error(`The i18n key "${key}" needs a value for {${name}}.`);
      }
      return String(params[name]);
    });
  }

  return { lang, t, plain: (key, params) => t(key, params).replace(EN_MARKER, '') };
}
