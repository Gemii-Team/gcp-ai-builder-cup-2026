/** A label whose wording depends on a count. English has two forms; Thai has none, so its two entries repeat one text. */
export type Plural = { one: string; other: string };

/**
 * Every fixed string of the `@kiwi/ui` components, English: the source of truth. `{name}` marks a parameter. Strings
 * rendered on screen go through `labels.node` (Thai gets its `lang` runs, `<en>Latin</en>` its `lang="en"`); strings
 * that must be plain text, such as an `aria-label`, go through `labels.text`.
 */
export const en = {
  // Proof slip
  generated: 'Generated {time}',
  sources: 'Sources',
  draftsToConfirm: { one: '{count} draft to confirm', other: '{count} drafts to confirm' },
  noDraftsWaiting: 'No drafts waiting',
  decision: 'Decision',
  openQuestion: 'Open question',

  // Failed
  failed: 'Failed {time}',
  retry: 'Retry',

  // Pending
  answerProgress: 'Answer progress',
  retrieving: { one: 'Retrieving from {count} channel…', other: 'Retrieving from {count} channels…' },
  retrieved: { one: 'Retrieved from {count} channel', other: 'Retrieved from {count} channels' },
  writingAnswer: 'Writing the answer',
  writingAnswerNow: 'Writing the answer…',

  // Offset, page and message links; citation markers
  transcriptAt: 'Transcript at {time}',
  pageShort: 'p. {page}',
  pageOf: '{file}, page {page}',
  messageIn: 'Message in #{channel} at {time}',
  messageInUntimed: 'Message in #{channel}',
  sourceN: 'Source {n}',
  sourceNOf: 'Source {n}: {label}',

  // Transcript excerpt
  close: 'Close',
  excerptLoading: 'Loading the excerpt…',
  excerptError: "Couldn't load this excerpt.",
  transcriptExcerptAt: 'Transcript excerpt at {time}',
  transcriptExcerpt: 'Transcript excerpt',
  documentExcerpt: 'Document excerpt, {file} page {page}',
  documentExcerptNoPage: 'Document excerpt, {file}',
  messageExcerpt: 'Message excerpt from {author}',
  transcriptHeader: 'Transcript · {title} · {time}',
  transcriptHeaderUntimed: 'Transcript · {title}',
  messageHeader: 'Message · #{channel} · {day} {time}',
  messageHeaderToday: 'Message · #{channel} · {time}',
  messageHeaderUntimed: 'Message · #{channel}',
  documentHeader: 'Document · {file} · p. {page}',
  documentPage: 'Page {page} of {pages} · {ref}',
  documentPageNoCount: 'Page {page} · {ref}',

  // Seal
  confirmed: 'Confirmed',
  confirmedStamp: 'CONFIRMED',

  // Draft slip and owner select
  draft: 'Draft',
  confirmAction: 'Confirm',
  confirmNamed: 'Confirm: {title}',
  edit: 'Edit',
  editNamed: 'Edit: {title}',
  save: 'Save',
  cancel: 'Cancel',
  taskTitle: 'Task title',
  titleEmpty: "Title can't be empty.",
  owner: 'Owner',
  chooseOwner: 'Choose an owner',
  chooseOwnerFirst: 'Choose an owner first.',
  noOwner: 'No owner',
  noDueDate: 'No due date',
  fromMeeting: 'From meeting',
  fromMessage: 'From message',
  fromAnswer: 'From answer {ref}',
  handoffNote: 'Stays a draft until a person confirms it on the Board.',
  reviewOnBoard: 'Review on the Board',
  discard: 'Discard',
  confirmFailed: "Couldn't confirm this draft. Try again.",
  saveFailed: "Couldn't save the change. Try again.",

  // Sealed slip
  confirmedBy: 'Confirmed by',

  // Drafts panel, band and bar
  aiDrafts: 'AI drafts',
  draftsCaption: 'Nothing here takes effect until you confirm it.',
  toConfirm: '{count} to confirm',
  allConfirmed: 'All confirmed',
  draftsList: 'Drafts to confirm',
  confirmedJustNow: 'Confirmed just now',
  draftsEmptyBody: 'Every AI draft has been confirmed.',
  board: 'Board',
  todo: 'To do',
  doing: 'Doing',
  done: 'Done',
  confirmedAnnouncement: {
    one: 'Confirmed: {title}. {count} draft left.',
    other: 'Confirmed: {title}. {count} drafts left.',
  },
  confirmedAnnouncementLast: 'Confirmed: {title}. All drafts confirmed.',

  // Task form and created card
  taskFieldTitle: 'Title',
  taskFieldChannel: 'Channel',
  dueDate: 'Due date',
  createTask: 'Create task',
  createFailed: "Couldn't create the task. Try again.",
  taskNamed: 'Task {code}',
  taskCreated: 'Task {code} created · {link}',
  viewOnBoard: 'View on Board',
};

/** The shape every language table must fill: each key of `en`, a plural key with both forms. */
export type UiLabels = { [Key in keyof typeof en]: (typeof en)[Key] extends string ? string : Plural };
