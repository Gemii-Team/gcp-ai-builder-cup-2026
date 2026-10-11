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
  signInNotInThisWorkspace: "isn't part of this Google Workspace. Sign in with your work account.",
  signInErrorStateMismatch: 'That sign-in expired. Try again.',
  signInErrorDomainNotAllowed: 'Use your company Google Workspace account.',
  signInErrorAccessDenied: 'Google sign-in was cancelled. Try again.',
  signInErrorExchangeFailed: "Couldn't finish signing in with Google. Try again.",
  languageLabel: 'Language',

  // Shell: rail, tab bar and account
  loading: 'Loading…',
  retry: 'Retry',
  close: 'Close',
  railLabel: 'Views, channels and agents',
  railViews: 'Views',
  railChannels: 'Channels',
  railAgents: 'Agents',
  navBoard: 'Board',
  navAsk: 'Ask',
  agentOnline: 'online',
  unreadMessages: { one: '{count} unread message', other: '{count} unread messages' },
  tabsLabel: 'Main navigation',
  tabChat: 'Chat',
  tabMe: 'Me',
  accountLabel: 'Account',
  signedInAs: 'Signed in as',
  signOut: 'Sign out',
  signOutFailed: "Couldn't sign out. Try again.",
  noChannels: "You aren't in any channel yet.",
  appLoadFailed: "Couldn't load K.I.W.I. Check your connection and try again.",
  channelsLoadFailed: "Couldn't load your channels.",

  // Channel: header and stream
  back: 'Back',
  membersCount: { one: '{count} member', other: '{count} members' },
  startMeeting: 'Start meeting',
  startMeetingFailed: "Couldn't start the meeting. Try again.",
  channelUnavailable: "This channel isn't available to you.",
  goToChannels: 'Go to your channels',
  messagesIn: 'Messages in #{channel}',
  loadingMessages: 'Loading messages…',
  loadMessagesFailed: "Couldn't load the messages.",
  loadEarlier: 'Load earlier',
  loadEarlierFailed: "Couldn't load earlier messages.",
  today: 'Today · {day}',
  kindMeetingSummary: 'meeting summary',
  kindFileSummary: 'file summary',
  kindChannelSummary: 'channel summary',
  deliverySending: 'Sending…',
  deliveryQueued: 'Waiting for the connection',
  deliveryFailed: 'Not sent',
  retrySend: 'Retry sending',
  summarizingFile: 'Summarizing file…',
  summarizingRecording: 'Summarizing the recording…',
  fileSummaryFailed: "Couldn't summarize {name}: {reason}",
  meetingSummaryFailed: "Couldn't summarize {title}: {reason}",
  reasonTooLarge: 'the file is too large.',
  reasonUnsupportedType: "this file type isn't supported.",
  reasonEmpty: 'the file is empty.',
  reasonUnreadable: 'no text could be read from it.',
  reasonNoSpeech: 'no speech was found in the recording.',
  reasonTranscriptTooLong: 'the transcript is too long.',
  reasonModelUnavailable: 'the AI service was unavailable. Try again.',
  reasonUnknown: 'something went wrong.',

  // Channel: composer, mentions and uploads
  composerLabel: 'Message #{channel}',
  composerPlaceholder: 'Message #{channel} · type @ to call an agent',
  composerPlaceholderPhone: 'Message #{channel}',
  attachFile: 'Attach file',
  sendMessage: 'Send',
  mentionLabel: 'Mention an agent or member',
  assistantRole: 'AI assistant · online',
  uploading: 'Uploading {name}…',
  uploadUnsupported: "Couldn't upload {name}: this file type isn't supported.",
  uploadTooLarge: "Couldn't upload {name}: the file is too large.",
  uploadEmpty: "Couldn't upload {name}: the file is empty.",
  uploadRejected: "Couldn't upload {name}: the server doesn't accept this file.",
  uploadFailed: "Couldn't upload {name}. Try again.",

  // Channel: Make a task and the drafts sheet
  makeTask: 'Make a task',
  makeTaskNamed: "Make a task from {author}'s message at {time}",
  makeTaskHeading: 'Make a task from this message',
  fromChannel: 'From #{channel}',
  followUpOn: 'Follow up on {file}',
  taskMadeAnnouncement: 'Task {code} created: {title}. It is on the Board under To do.',
  aiDrafts: 'AI drafts',
  summarizedBy: 'Summarized by K.I.W.I.',
  retryFailed: "Couldn't start the retry. Try again.",
};

/** The shape every language table must fill: each key of `en`, a plural key with both forms. */
export type I18nTable = { [Key in keyof typeof en]: (typeof en)[Key] extends string ? string : Plural };
