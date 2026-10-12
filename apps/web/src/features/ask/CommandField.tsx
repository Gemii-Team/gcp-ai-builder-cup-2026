import { Button, Icon, isImeKey } from '@kiwi/ui';
import { useEffect, type RefObject } from 'react';
import { T, useI18n } from '../../i18n';
import { useMediaQuery } from '../../lib/useMediaQuery';

type CommandFieldProps = {
  value: string;
  onChange: (value: string) => void;
  /** Asks the question: always trimmed and never empty. */
  onSubmit: (question: string) => void;
  /** The element that says what is searched: the field is described by it. */
  describedBy?: string;
  inputRef: RefObject<HTMLInputElement | null>;
};

/**
 * The command field: the largest control of the screen (56px, 48px on phones). `/` focuses it from anywhere that is not a
 * field, Enter asks, Escape clears it, and neither ever acts on the key that commits or dismisses an IME candidate. Ask is
 * disabled until there is text, and its reason stays available to assistive tech.
 */
export function CommandField({ value, onChange, onSubmit, describedBy, inputRef }: CommandFieldProps) {
  const { plain } = useI18n();
  const phone = useMediaQuery('(max-width: 1023px)');
  const empty = value.trim() === '';

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const inField = event.target instanceof Element && event.target.closest('input, textarea, select, [contenteditable]') !== null;
      if (event.key !== '/' || event.metaKey || event.ctrlKey || event.altKey || event.isComposing || event.defaultPrevented || inField) {
        return;
      }
      event.preventDefault();
      inputRef.current?.focus();
    };
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('keydown', onKeyDown);
    };
  }, [inputRef]);

  function ask() {
    if (!empty) {
      onSubmit(value.trim());
    }
  }

  return (
    <form
      className="ak-qf"
      role="search"
      autoComplete="off"
      data-has={value !== '' || undefined}
      onSubmit={(event) => {
        event.preventDefault();
        ask();
      }}
    >
      <Icon name="search" />
      <input
        ref={inputRef}
        type="text"
        aria-label={plain('askInputLabel')}
        aria-describedby={describedBy}
        enterKeyHint="search"
        spellCheck={false}
        placeholder={plain(phone ? 'askPlaceholderPhone' : 'askPlaceholder')}
        value={value}
        onChange={(event) => {
          onChange(event.target.value);
        }}
        onKeyDown={(event) => {
          if (isImeKey(event)) {
            // The Enter that commits a candidate must not send the form, and the Escape that dismisses one must not clear it.
            if (event.key === 'Enter') {
              event.preventDefault();
            }
            return;
          }
          if (event.key === 'Enter') {
            event.preventDefault();
            ask();
          } else if (event.key === 'Escape') {
            if (value === '') {
              event.currentTarget.blur();
            } else {
              onChange('');
            }
          }
        }}
      />
      <kbd className="ak-kbd" aria-hidden="true">
        /
      </kbd>
      <Button variant="primary" type="submit" disabled={empty} disabledReason={plain('askNeedText')}>
        <Icon name="send" />
        <span className="ak-lbl">
          <T k="askSubmit" />
        </span>
      </Button>
    </form>
  );
}
