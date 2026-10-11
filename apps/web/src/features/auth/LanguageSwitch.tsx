import type { Lang } from '@kiwi/core';
import { Segmented } from '@kiwi/ui';
import { useI18n } from '../../i18n';
import { useLanguage } from '../../state/ui';

// Each segment names its language in that language, and is tagged for it, whatever the interface language is.
const OPTIONS = [
  { value: 'en', label: 'English', lang: 'en' },
  { value: 'th', label: 'ไทย', lang: 'th' },
] as const satisfies readonly { value: Lang; label: string; lang: string }[];

/** The English / ไทย switch of the sign-in page's top bar. The choice persists and sets the document language. */
export function LanguageSwitch() {
  const { plain } = useI18n();
  const [lang, setLang] = useLanguage();
  return <Segmented label={plain('languageLabel')} value={lang} onChange={setLang} options={OPTIONS} />;
}
