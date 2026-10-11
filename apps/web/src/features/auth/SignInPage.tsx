import './signin.css';
import { Mark, Mascot } from '@kiwi/ui';
import { useEffect } from 'react';
import { T, useI18n } from '../../i18n';
import { LanguageSwitch } from './LanguageSwitch';
import { SignInCard } from './SignInCard';

/** Counter Mint (`--color-counter-mint`): the sign-in page has no teal rail, so the browser chrome matches the ground. */
const COUNTER_MINT = '#E6F2EE';

/** While the page is mounted the browser's theme colour is Counter Mint; the previous colour (the rail's teal) returns after. */
function useThemeColor(colour: string) {
  useEffect(() => {
    const existing = document.querySelector<HTMLMetaElement>('meta[name="theme-color"]');
    const meta = existing ?? document.head.appendChild(Object.assign(document.createElement('meta'), { name: 'theme-color' }));
    const previous = meta.content;
    meta.content = colour;
    return () => {
      if (existing === null) {
        meta.remove();
      } else {
        meta.content = previous;
      }
    };
  }, [colour]);
}

/** The document title follows the interface language while the page is mounted. */
function useDocumentTitle(title: string) {
  useEffect(() => {
    const previous = document.title;
    document.title = title;
    return () => {
      document.title = previous;
    };
  }, [title]);
}

type SignInPageProps = { returnTo: string; error?: string; email?: string; signedOut: boolean };

/**
 * `/signin`: no rail and no tab bar. The language switch at the top right, the brand lockup with its tagline, one slip,
 * the kiwi beside it from 1100px up, and the demo label, which every screen carries.
 */
export function SignInPage(props: SignInPageProps) {
  const { plain } = useI18n();
  useDocumentTitle(plain('signInDocumentTitle'));
  useThemeColor(COUNTER_MINT);

  return (
    <div className="si-page">
      <header className="si-top">
        <LanguageSwitch />
      </header>
      <main className="si-main">
        <div className="si-brand">
          <div className="si-lock">
            <Mark size={40} />
            <strong lang="en">K.I.W.I.</strong>
          </div>
          <p>
            <T k="signInTagline" />
          </p>
        </div>
        <div className="si-stage">
          <Mascot variant="plain" size={112} className="si-kiwi" />
          <SignInCard {...props} />
        </div>
      </main>
      <p className="si-demo">
        Demo data · <span lang="th">ข้อมูลตัวอย่าง</span>
      </p>
    </div>
  );
}
