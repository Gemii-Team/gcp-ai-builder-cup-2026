import { afterEach, describe, expect, it, vi } from 'vitest';
import { en, type Plural } from './en';
import { createI18n, type I18nKey } from './index';
import { th } from './th';

type Table = Record<string, string | Plural>;

/** Every string of a table by key path: a plural key contributes `key.one` and `key.other`. */
function entries(table: Table): [string, string][] {
  return Object.entries(table).flatMap(([key, value]) =>
    typeof value === 'string'
      ? ([[key, value]] as [string, string][])
      : ([
          [`${key}.one`, value.one],
          [`${key}.other`, value.other],
        ] as [string, string][]),
  );
}

const placeholders = (text: string) => [...text.matchAll(/\{(\w+)\}/g)].map((match) => match[1]).sort();

afterEach(() => {
  vi.unstubAllEnvs();
});

describe('the tables', () => {
  it('TestI18n_ThaiHasEveryEnglishKey', () => {
    const english = entries(en);
    const thai = entries(th);
    expect(thai.map(([key]) => key).sort()).toEqual(english.map(([key]) => key).sort());
    for (const [key, value] of [...english, ...thai]) {
      expect(value.trim(), key).not.toBe('');
    }
    for (const [key, value] of Object.entries(en as Table)) {
      expect(typeof (th as Table)[key], key).toBe(typeof value);
    }
  });

  it('gives each Thai string the same {placeholders} as its English source, so no parameter is dropped', () => {
    const thai = new Map(entries(th));
    for (const [key, value] of entries(en)) {
      expect(placeholders(thai.get(key) ?? ''), key).toEqual(placeholders(value));
    }
  });

  it('writes Thai that is Thai: every Thai string has a Thai character', () => {
    for (const [key, value] of entries(th)) {
      expect(value, key).toMatch(/[\u0E00-\u0E7F]/);
    }
  });

  it('TestI18n_ThaiStringsWrapEveryLatinRunInEnMarkers', () => {
    for (const [key, value] of entries(th)) {
      // What is left once the marked runs and the {placeholders} are taken out must hold no Latin letter.
      const unmarked = value.replace(/<en>[^<]*<\/en>/g, '').replace(/\{\w+\}/g, '');
      expect(unmarked, key).not.toMatch(/[A-Za-z]/);
      for (const [, latin] of value.matchAll(/<en>([^<]*)<\/en>/g)) {
        expect(latin, key).toMatch(/[A-Za-z]/);
      }
    }
  });

  it('leaves English unmarked: English is the document language', () => {
    for (const [key, value] of entries(en)) {
      expect(value, key).not.toMatch(/<\/?en>/);
    }
  });

  it('has English plural forms and no Thai ones: Thai repeats one text for both', () => {
    expect(en.draftsToConfirm.one).not.toBe(en.draftsToConfirm.other);
    expect(th.draftsToConfirm.one).toBe(th.draftsToConfirm.other);
  });

  it('says in Thai that no answer with a source was received (ไม่ได้รับ: "did not receive"), not that none was obtained', () => {
    expect(createI18n('th').plain('reasonNoAnswer')).toBe('ไม่ได้รับคำตอบที่มีแหล่งที่มา ลองอีกครั้ง');
  });
});

describe('createI18n', () => {
  it('TestI18n_PluralDraftsToConfirm', () => {
    const { t } = createI18n('en');

    expect(t('draftsToConfirm', { count: 1 })).toBe('1 draft to confirm');
    expect(t('draftsToConfirm', { count: 3 })).toBe('3 drafts to confirm');
    expect(t('draftsToConfirm', { count: 0 })).toBe('0 drafts to confirm');
  });

  it('has no plural split in Thai', () => {
    const { t } = createI18n('th');

    expect(t('draftsToConfirm', { count: 1 })).toBe('1 ฉบับร่างรอยืนยัน');
    expect(t('draftsToConfirm', { count: 3 })).toBe('3 ฉบับร่างรอยืนยัน');
  });

  it('TestI18n_MissingKeyThrows', () => {
    for (const lang of ['en', 'th'] as const) {
      expect(() => createI18n(lang).t('noSuchKey' as I18nKey), lang).toThrow('noSuchKey');
      expect(() => createI18n(lang).plain('noSuchKey' as I18nKey), lang).toThrow('noSuchKey');
    }
  });

  it('shows the key itself rather than crash on a missing key in production', () => {
    vi.stubEnv('DEV', false);

    expect(createI18n('th').t('noSuchKey' as I18nKey)).toBe('noSuchKey');
  });

  it('throws when a placeholder has no value, so a screen never shows a literal {name}', () => {
    const { t } = createI18n('en');

    expect(() => t('signInTitle')).toThrow('workspace');
    expect(() => t('signInTitle', { other: 'x' })).toThrow('workspace');
    expect(() => t('draftsToConfirm')).toThrow('count');
  });

  it('fills {placeholders} from the params', () => {
    expect(createI18n('en').t('signInTitle', { workspace: 'Siam Riverside Trading' })).toBe('Sign in to Siam Riverside Trading');
    expect(createI18n('th').t('signInTitle', { workspace: 'Siam Riverside Trading' })).toBe('ลงชื่อเข้าใช้ Siam Riverside Trading');
  });

  it('defaults to English', () => {
    const { lang, t } = createI18n();

    expect(lang).toBe('en');
    expect(t('signInContinueWithGoogle')).toBe('Continue with Google');
  });

  it('keeps the <en> markers in t, for Rich, and drops them in plain, for an attribute or the title', () => {
    const i18n = createI18n('th');

    expect(i18n.t('signInContinueWithGoogle')).toBe('ดำเนินการต่อด้วย <en>Google</en>');
    expect(i18n.plain('signInContinueWithGoogle')).toBe('ดำเนินการต่อด้วย Google');
    expect(i18n.plain('signInDocumentTitle')).toBe('ลงชื่อเข้าใช้ · K.I.W.I.');
  });
});
