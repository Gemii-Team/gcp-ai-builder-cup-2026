import { Icon, IconButton, LangRuns, Rich } from '@kiwi/ui';
import { useRef } from 'react';
import { useI18n } from '../../i18n';
import type { UploadRejection, UploadState } from './useUpload';

/** What the paperclip offers the file picker: the attachments the server takes, and sound and video, which become a meeting. */
const ACCEPT = '.pdf,.png,.jpg,.jpeg,.txt,.md,.csv,audio/*,video/*';

/** The paperclip: a 44px icon button on phones that opens the file picker (the input itself stays out of sight). */
export function AttachButton({ onPick }: { onPick: (file: File) => void }) {
  const { plain } = useI18n();
  const input = useRef<HTMLInputElement>(null);
  return (
    <>
      <IconButton
        label={plain('attachFile')}
        onClick={() => {
          input.current?.click();
        }}
      >
        <Icon name="paperclip" />
      </IconButton>
      <input
        ref={input}
        type="file"
        hidden
        tabIndex={-1}
        aria-label={plain('attachFile')}
        accept={ACCEPT}
        onChange={(event) => {
          const file = event.target.files?.[0];
          if (file !== undefined) {
            onPick(file);
          }
          // The same file may be picked again after a rejection.
          event.target.value = '';
        }}
      />
    </>
  );
}

/** Stands where the file name goes while the sentence around it is rendered (a control character no sentence contains). */
const NAME_SLOT = '\u0001';

/**
 * One of the sentences that name a file. Its Latin words in Thai (`WAV`, `PCM16`) are tagged `lang="en"` by `Rich`, and
 * the file name, which is the person's own words, has its Thai runs tagged `lang="th"` by `LangRuns`.
 */
function Sentence({ rejection, name }: { rejection: UploadRejection; name: string }) {
  const { t } = useI18n();
  const text = t(rejection.key, { name: NAME_SLOT, ...('reason' in rejection ? { reason: t(rejection.reason) } : {}) });
  const [before = '', after = ''] = text.split(NAME_SLOT);
  return (
    <>
      <Rich text={before} />
      <LangRuns text={name} />
      <Rich text={after} />
    </>
  );
}

/** Above the composer: the file being sent, or why the server would not take it, in the one failure family. */
export function UploadNotice({ state, onDismiss }: { state: UploadState; onDismiss: () => void }) {
  const { plain } = useI18n();
  if (state.status === 'idle') {
    return null;
  }
  if (state.status === 'uploading') {
    return (
      <p className="delivery" role="status">
        <Icon name="clock" />
        <LangRuns text={plain('uploading', { name: state.name })} />
      </p>
    );
  }
  return (
    <p className="notice" role="alert">
      <Icon name="warning" />
      <span>
        <Sentence rejection={state.rejection} name={state.name} />
      </span>
      <IconButton label={plain('close')} className="notice-close" onClick={onDismiss}>
        <Icon name="close" />
      </IconButton>
    </p>
  );
}
