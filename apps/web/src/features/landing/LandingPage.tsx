import './landing.css';
import { Icon, Mark, Mascot, Seal, type IconName } from '@kiwi/ui';
import { useEffect } from 'react';
import type { I18nKey } from '@kiwi/core';
import { T, useI18n } from '../../i18n';
import { LanguageSwitch } from '../auth/LanguageSwitch';
import { AppButtonLink, AppLink } from '../shell/AppLink';

/** The six things K.I.W.I. does, in the order the demo shows them. */
const FEATURES: readonly { icon: IconName; title: I18nKey; body: I18nKey }[] = [
  { icon: 'video', title: 'landingFeatureMinutesTitle', body: 'landingFeatureMinutesBody' },
  { icon: 'task-add', title: 'landingFeatureTasksTitle', body: 'landingFeatureTasksBody' },
  { icon: 'ask', title: 'landingFeatureAskTitle', body: 'landingFeatureAskBody' },
  { icon: 'chat', title: 'landingFeatureChatTitle', body: 'landingFeatureChatBody' },
  { icon: 'clock', title: 'landingFeatureCalendarTitle', body: 'landingFeatureCalendarBody' },
  { icon: 'user', title: 'landingFeaturePrivateTitle', body: 'landingFeaturePrivateBody' },
];

const STEPS: readonly { title: I18nKey; body: I18nKey }[] = [
  { title: 'landingHowStep1Title', body: 'landingHowStep1Body' },
  { title: 'landingHowStep2Title', body: 'landingHowStep2Body' },
  { title: 'landingHowStep3Title', body: 'landingHowStep3Body' },
];

const TRUST: readonly I18nKey[] = ['landingTrustCloud', 'landingTrustGemini', 'landingTrustLanguages', 'landingTrustHuman'];

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

/**
 * `/welcome`: what K.I.W.I. is, for someone who has not signed in. A signed-out visit to `/` lands here; every way in
 * leads to `/signin`. Built from the Proof Slip vocabulary of the app itself (slips, perforations, REF codes, the
 * CONFIRMED seal), flat, with no gradients and no shadows.
 */
export function LandingPage() {
  const { plain } = useI18n();
  useDocumentTitle(plain('landingDocumentTitle'));

  return (
    <div className="lp-page">
      <header className="lp-top">
        <div className="lp-wrap lp-top-in">
          <AppLink to="/welcome" className="lp-lock" aria-label="K.I.W.I.">
            <Mark size={40} />
            <strong lang="en">K.I.W.I.</strong>
          </AppLink>
          <nav className="lp-nav" aria-label={plain('landingNavFeatures')}>
            <a href="#features">
              <T k="landingNavFeatures" />
            </a>
            <a href="#how">
              <T k="landingNavHow" />
            </a>
            <a href="#trust">
              <T k="landingNavTrust" />
            </a>
          </nav>
          <div className="lp-top-acts">
            <LanguageSwitch />
            <AppButtonLink to="/signin" variant="primary" size="sm">
              <T k="landingSignIn" />
            </AppButtonLink>
          </div>
        </div>
      </header>

      <main>
        <section className="lp-hero">
          <div className="lp-wrap lp-hero-in">
            <div className="lp-hero-copy">
              <p className="lp-eyebrow">
                <span className="lp-dot" aria-hidden="true" />
                <T k="landingEyebrow" />
              </p>
              <h1 className="lp-h1">
                <T k="landingHeadline" />
              </h1>
              <p className="lp-lede">
                <T k="landingLede" />
              </p>
              <div className="lp-ctas">
                <AppButtonLink to="/signin" variant="primary" className="lp-cta">
                  <T k="landingCtaPrimary" />
                  <Icon name="arrow-right" />
                </AppButtonLink>
                <a href="#how" className="lp-ghost">
                  <T k="landingCtaSecondary" />
                </a>
              </div>
            </div>
            <ProductPreview label={plain('landingDemoVisualLabel')} />
          </div>
        </section>

        <section id="trust" className="lp-trust" aria-label={plain('landingNavTrust')}>
          <ul className="lp-wrap lp-trust-in">
            {TRUST.map((key) => (
              <li key={key}>
                <Icon name="check" size={16} />
                <T k={key} />
              </li>
            ))}
          </ul>
        </section>

        <section id="features" className="lp-section">
          <div className="lp-wrap">
            <header className="lp-head">
              <h2 className="lp-h2">
                <T k="landingFeaturesTitle" />
              </h2>
              <p className="lp-sub">
                <T k="landingFeaturesLede" />
              </p>
            </header>
            <ul className="lp-grid">
              {FEATURES.map((feature) => (
                <li key={feature.title} className="lp-card">
                  <span className="lp-card-ic" aria-hidden="true">
                    <Icon name={feature.icon} size={22} />
                  </span>
                  <h3 className="lp-h3">
                    <T k={feature.title} />
                  </h3>
                  <p>
                    <T k={feature.body} />
                  </p>
                </li>
              ))}
            </ul>
          </div>
        </section>

        <section id="how" className="lp-section lp-how">
          <div className="lp-wrap">
            <header className="lp-head">
              <h2 className="lp-h2">
                <T k="landingHowTitle" />
              </h2>
            </header>
            <ol className="lp-steps">
              {STEPS.map((step, index) => (
                <li key={step.title} className="lp-step">
                  <span className="lp-step-n" aria-hidden="true">
                    {String(index + 1).padStart(2, '0')}
                  </span>
                  <h3 className="lp-h3">
                    <T k={step.title} />
                  </h3>
                  <p>
                    <T k={step.body} />
                  </p>
                </li>
              ))}
            </ol>
            <div className="lp-principle">
              <Seal className="lp-seal" />
              <div>
                <p className="lp-principle-t">
                  <T k="landingPrincipleTitle" />
                </p>
                <p>
                  <T k="landingPrincipleBody" />
                </p>
              </div>
            </div>
          </div>
        </section>

        <section className="lp-final">
          <div className="lp-wrap lp-final-in">
            <span className="lp-final-kiwi" aria-hidden="true">
              <Mascot variant="plain" size={96} />
            </span>
            <div>
              <h2 className="lp-h2 lp-on-ink">
                <T k="landingFinalTitle" />
              </h2>
              <p className="lp-final-sub">
                <T k="landingFinalBody" />
              </p>
            </div>
            <AppButtonLink to="/signin" className="lp-final-cta">
              <T k="landingCtaPrimary" />
              <Icon name="arrow-right" />
            </AppButtonLink>
          </div>
        </section>
      </main>

      <footer className="lp-foot">
        <div className="lp-wrap lp-foot-in">
          <span>
            <T k="landingFooter" />
          </span>
          <span className="lp-demo">
            <span lang="en">Demo data</span> · <span lang="th">ข้อมูลตัวอย่าง</span>
          </span>
        </div>
      </footer>
    </div>
  );
}

/**
 * The hero's picture of the product: three slips as the app draws them (a meeting's minutes, the draft task it left,
 * and a cited answer). Decorative, so it is one labelled image to assistive technology.
 */
function ProductPreview({ label }: { label: string }) {
  return (
    <div className="lp-preview" role="img" aria-label={label}>
      <div className="lp-slip lp-slip-minutes" aria-hidden="true">
        <div className="lp-slip-head">
          <span className="lp-slip-ic">
            <Icon name="video" size={16} />
          </span>
          <strong>
            <T k="landingDemoMinutesTitle" />
          </strong>
          <span className="lp-ref">REF MTG-1001</span>
        </div>
        <p className="lp-slip-body">
          <T k="landingDemoMinutesBody" />
        </p>
        <div className="lp-row">
          <span className="lp-row-k">
            <T k="landingDemoDecision" />
          </span>
          <span className="lp-row-v">
            <T k="landingDemoDecisionText" />
          </span>
          <span className="lp-time">0:01:40</span>
        </div>
        <div className="lp-row">
          <span className="lp-row-k lp-row-q">
            <T k="landingDemoQuestion" />
          </span>
          <span className="lp-row-v">
            <T k="landingDemoQuestionText" />
          </span>
          <span className="lp-time">0:03:20</span>
        </div>
      </div>

      <div className="lp-slip lp-slip-draft" aria-hidden="true">
        <div className="lp-slip-head">
          <strong>
            <T k="landingDemoDraftTitle" />
          </strong>
          <span className="lp-tag">
            <T k="landingDemoDraftTag" />
          </span>
        </div>
        <div className="lp-perf" />
        <div className="lp-slip-foot">
          <span className="lp-ref">REF TSK-1003</span>
          <span className="lp-confirm">
            <Icon name="check" size={14} />
            <T k="landingDemoConfirm" />
          </span>
        </div>
      </div>

      <div className="lp-slip lp-slip-answer" aria-hidden="true">
        <div className="lp-slip-head">
          <span className="lp-slip-ic">
            <Icon name="ask" size={16} />
          </span>
          <span className="lp-ref">REF ANS-1004</span>
        </div>
        <p className="lp-slip-body">
          <T k="landingDemoAnswer" /> <span className="lp-cite">[1]</span>
        </p>
      </div>
    </div>
  );
}
