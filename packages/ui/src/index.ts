// The package's stylesheets are `@kiwi/ui/theme.css` and `@kiwi/ui/fonts.css` (see package.json `exports`); the
// components below draw with the theme's tokens. The proof slip family brings its own stylesheet.
import './slip/slip.css';

export { Button } from './Button';
export { dateToDueAt, dueAtToDate, formatClock, formatDay, formatOffset, formatSize } from './format';
export { Icon, type IconName } from './icons';
export { IconButton } from './IconButton';
export { EnMarker, LangRuns, Rich, langRuns } from './LangRuns';
export { Mark, MarkSmall, Mascot, Seal } from './marks';
export { Menu, MenuItem, MenuRadioItem } from './Menu';
export { Numeral } from './Numeral';
export { Segmented } from './Segmented';

export { CitationMarker } from './slip/CitationMarker';
export { ExcerptProvider, ExcerptRow, type Excerpt, type OpenExcerpt, type Source } from './slip/ExcerptContext';
export { FailedSlip } from './slip/FailedSlip';
export { FileChip } from './slip/FileChip';
export { Perforation } from './slip/Perforation';
export { Pending, PendingBar, usePrefersReducedMotion } from './slip/Pending';
export { PendingSlip } from './slip/PendingSlip';
export { PendingSteps } from './slip/PendingSteps';
export { ProofSlip } from './slip/ProofSlip';
export { RefCode } from './slip/RefCode';
export { Slip, SlipBody } from './slip/Slip';
export { SourceExcerpt } from './slip/SourceExcerpt';
export { MessageLink, OffsetLink, PageLink } from './slip/SourceLinks';
export { SourceRow } from './slip/SourceRow';
export { Stub, StubRow } from './slip/Stub';
export { Tracks, type Track } from './slip/Tracks';
