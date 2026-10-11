/** A string whose wording depends on a count. English has two forms; Thai has one, so its two entries repeat one text. */
export type Plural = { one: string; other: string };

/**
 * Every fixed string of the web screens, English: the source of truth, and the language of the interface unless a
 * person picks Thai. `{name}` marks a parameter (`{count}` also picks the plural form). The strings of the `@kiwi/ui`
 * components live in that package's own label tables, not here. A screen adds its strings here and to `th.ts` together.
 */
export const en = {
  // Shared
  draftsToConfirm: { one: '{count} draft to confirm', other: '{count} drafts to confirm' },

  // Sign in
  signInDocumentTitle: 'Sign in · K.I.W.I.',
  signInTagline: "Your team's channels, meetings and files, with AI that shows its sources.",
  signInTitle: 'Sign in to {workspace}',
  signInTitleDefault: 'Sign in to K.I.W.I.',
  signInContinueWithGoogle: 'Continue with Google',
  signInRedirecting: 'Redirecting to Google…',
  signInHint: 'Use your {domain} Google account.',
  signInSeeHeading: 'What K.I.W.I. will see',
  signInSeeChannels: 'Channels you join and messages in them',
  signInSeeFiles: 'Files you drop into a channel',
  signInSeeMeet: 'Meet transcripts from meetings you start in K.I.W.I.',
  signInSeeAsks: 'K.I.W.I. asks before it reads Drive or drafts Gmail.',
  signInNotInWorkspace: "isn't part of {workspace}'s Google Workspace. Sign in with your work account.",
  signInSignedOut: "You've signed out.",
};

/** The shape every language table must fill: each key of `en`, a plural key with both forms. */
export type I18nTable = { [Key in keyof typeof en]: (typeof en)[Key] extends string ? string : Plural };
