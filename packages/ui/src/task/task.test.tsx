// @vitest-environment jsdom
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState, type ReactElement } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Member } from '../draft/OwnerSelect';
import { en } from '../labels/en';
import { LabelsProvider, type UiLang } from '../labels';
import { CreatedTaskCard, type CreatedTaskCardProps } from './CreatedTaskCard';
import { DateField } from './DateField';
import { TaskForm, type TaskFormProps, type TaskFormValues } from './TaskForm';

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const somchai: Member = { id: 'u1', name: 'Somchai W.' };
const nok: Member = { id: 'u2', name: 'Nok' };
const arthit: Member = { id: 'u3', name: 'Arthit K.' };

const channels = [
  { id: 'c1', name: 'q4-launch' },
  { id: 'c2', name: 'sales-th' },
  { id: 'c3', name: 'general' },
];
const rosters: Record<string, Member[]> = { c1: [somchai, nok], c2: [arthit], c3: [nok, arthit] };

type FormProps = Partial<TaskFormProps>;

/** Renders the form and waits for the Owner options of its first channel, so no update lands after the test. */
async function renderForm(props: FormProps = {}) {
  const membersFor = vi.fn((channelId: string) => Promise.resolve(rosters[channelId] ?? []));
  const onSubmit = vi.fn((values: TaskFormValues) => {
    void values;
    return Promise.resolve();
  });
  const onCancel = vi.fn();
  const view = render(
    <TaskForm
      heading="Make a task from this message"
      subtitle="From #q4-launch · Arthit K. · 14:02"
      initialTitle="Send the quotation to ACME"
      channelId="c1"
      membersFor={membersFor}
      onSubmit={onSubmit}
      onCancel={onCancel}
      {...props}
    />,
  );
  await screen.findByRole('option', { name: 'Nok' });
  return { ...view, membersFor, onSubmit, onCancel };
}

const titleField = () => screen.getByRole('textbox', { name: 'Title' });
const ownerSelect = () => screen.getByRole('combobox', { name: 'Owner' });

describe('TaskForm', () => {
  it('TaskForm_PrefillsSelectsAndFocusesTheTitle', async () => {
    await renderForm();
    const title = titleField();
    expect(title).toHaveValue('Send the quotation to ACME');
    expect(title).toHaveFocus();
    expect(title).toHaveProperty('selectionStart', 0);
    expect(title).toHaveProperty('selectionEnd', 'Send the quotation to ACME'.length);
    expect(title).toHaveAttribute('maxlength', '140');
    expect(screen.getByRole('form', { name: 'Make a task from this message' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Make a task from this message' })).toBeInTheDocument();
    expect(screen.getByText('From #q4-launch · Arthit K. · 14:02')).toBeInTheDocument();
    // The message form has no Channel select; the Board's form does.
    expect(screen.queryByRole('combobox', { name: 'Channel' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Create task' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeInTheDocument();
  });

  it('TaskForm_EmptyTitle_InvalidWithMessageAndKeepsFocus', async () => {
    const user = userEvent.setup();
    const { onSubmit } = await renderForm();
    const title = titleField();
    await user.clear(title);
    await user.click(screen.getByRole('button', { name: 'Create task' }));

    expect(title).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByText("Title can't be empty.")).toBeInTheDocument();
    expect(title).toHaveAccessibleDescription("Title can't be empty.");
    expect(title).toHaveFocus();
    expect(onSubmit).not.toHaveBeenCalled();

    // A title of spaces is empty too, and typing clears the invalid look.
    await user.type(title, '   ');
    await user.keyboard('{Enter}');
    expect(title).toHaveAttribute('aria-invalid', 'true');
    expect(onSubmit).not.toHaveBeenCalled();
    await user.type(title, 'x');
    expect(title).not.toHaveAttribute('aria-invalid');
    expect(screen.queryByText("Title can't be empty.")).toBeNull();
  });

  it('TaskForm_EscapeAndCancelCallOnCancel', async () => {
    const user = userEvent.setup();
    const { onCancel, onSubmit } = await renderForm();
    await user.keyboard('{Escape}');
    expect(onCancel).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onCancel).toHaveBeenCalledTimes(2);
    expect(onSubmit).not.toHaveBeenCalled();
  });

  // The Escape that dismisses a Thai IME candidate and the Enter that commits one belong to the IME, not to the form.
  it('TaskForm_ImeEscapeAndEnterDoNotCancelOrSubmit_ButAPlainEscapeStillCancels', async () => {
    const user = userEvent.setup();
    const { onCancel, onSubmit } = await renderForm();
    const title = screen.getByRole('textbox', { name: 'Title' });

    fireEvent.keyDown(title, { key: 'Escape', isComposing: true, keyCode: 229 });
    fireEvent.keyDown(title, { key: 'Escape', isComposing: false, keyCode: 229 });
    const enter = fireEvent.keyDown(title, { key: 'Enter', isComposing: true, keyCode: 229 });
    const safariEnter = fireEvent.keyDown(title, { key: 'Enter', isComposing: false, keyCode: 229 });

    expect(onCancel).not.toHaveBeenCalled();
    // The press is cancelled before it can become an implicit submit of the form.
    expect(enter).toBe(false);
    expect(safariEnter).toBe(false);
    expect(onSubmit).not.toHaveBeenCalled();

    await user.keyboard('{Escape}');
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('TaskForm_OwnerDefaultsToNoOwnerWithoutTheAmberWarningLook', async () => {
    await renderForm();
    const owner = ownerSelect();
    expect(owner).toHaveValue('');
    expect(within(owner).getByRole('option', { name: 'No owner' })).toBeInTheDocument();
    expect(within(owner).queryByRole('option', { name: 'Choose an owner' })).toBeNull();
    // An owner is optional here: no warning icon, no warning attribute, no amber class anywhere in the form.
    expect(owner).not.toHaveAttribute('data-warn');
    const form = screen.getByRole('form');
    expect(form.querySelector('[data-warn], [data-icon="warning"]')).toBeNull();
    expect(form.innerHTML).not.toMatch(/amber|draft|warn/i);
  });

  it('TaskForm_ChannelChangeRepopulatesOwnersAndClearsANonMember', async () => {
    const user = userEvent.setup();
    const { membersFor, onSubmit } = await renderForm({
      channels,
      heading: 'New task',
      subtitle: 'Added by hand: no meeting, message or document behind it.',
    });
    expect(membersFor).toHaveBeenLastCalledWith('c1');
    expect(within(ownerSelect()).getAllByRole('option').map((option) => option.textContent)).toEqual(['No owner', 'Somchai W.', 'Nok']);
    const channel = screen.getByRole('combobox', { name: 'Channel' });
    expect(channel).toHaveValue('c1');
    expect(within(channel).getAllByRole('option').map((option) => option.textContent)).toEqual(['# q4-launch', '# sales-th', '# general']);
    const create = screen.getByRole('button', { name: 'Create task' });
    // A native select shows its first option whenever its value matches none, so what the form holds is read from what it
    // submits: the select alone cannot tell a cleared owner from a stale one.
    const submitted = async (call: number) => {
      await user.click(create);
      await waitFor(() => {
        expect(onSubmit).toHaveBeenCalledTimes(call);
        expect(create).not.toHaveAttribute('aria-busy');
      });
    };

    await user.selectOptions(ownerSelect(), 'Nok');
    expect(ownerSelect()).toHaveValue('u2');

    // Nok is a member of #general too, so the choice stays while the options are fetched again.
    await user.selectOptions(channel, 'c3');
    expect(membersFor).toHaveBeenLastCalledWith('c3');
    await screen.findByRole('option', { name: 'Arthit K.' });
    expect(within(ownerSelect()).getAllByRole('option').map((option) => option.textContent)).toEqual(['No owner', 'Nok', 'Arthit K.']);
    expect(ownerSelect()).toHaveValue('u2');
    await submitted(1);
    expect(onSubmit).toHaveBeenNthCalledWith(1, { title: 'Send the quotation to ACME', channelId: 'c3', ownerId: 'u2', dueDate: null });

    // Nok is not in #sales-th: the options are replaced and the owner is cleared back to No owner.
    await user.selectOptions(channel, 'c2');
    await waitFor(() => {
      expect(within(ownerSelect()).queryByRole('option', { name: 'Nok' })).toBeNull();
    });
    expect(within(ownerSelect()).getAllByRole('option').map((option) => option.textContent)).toEqual(['No owner', 'Arthit K.']);
    expect(ownerSelect()).toHaveValue('');
    await submitted(2);
    expect(onSubmit).toHaveBeenNthCalledWith(2, { title: 'Send the quotation to ACME', channelId: 'c2', ownerId: null, dueDate: null });
  });

  it('TaskForm_SubmitsTitleChannelOwnerAndDueDate', async () => {
    const user = userEvent.setup();
    const { onSubmit } = await renderForm({ channels });
    await user.clear(titleField());
    await user.type(titleField(), '  Follow up with Globex ');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Channel' }), 'c3');
    await screen.findByRole('option', { name: 'Arthit K.' });
    await user.selectOptions(ownerSelect(), 'Arthit K.');
    await user.click(within(screen.getByRole('group', { name: 'Due date' })).getAllByRole('spinbutton')[0]);
    await user.keyboard('16102026');
    await user.click(screen.getByRole('button', { name: 'Create task' }));
    expect(onSubmit).toHaveBeenCalledExactlyOnceWith({ title: 'Follow up with Globex', channelId: 'c3', ownerId: 'u3', dueDate: '2026-10-16' });
  });

  it('submits no owner and no due date when none was chosen', async () => {
    const user = userEvent.setup();
    const { onSubmit } = await renderForm();
    await user.click(screen.getByRole('button', { name: 'Create task' }));
    expect(onSubmit).toHaveBeenCalledExactlyOnceWith({ title: 'Send the quotation to ACME', channelId: 'c1', ownerId: null, dueDate: null });
  });

  it('TaskForm_RejectedSubmitStaysOpenAndShowsTheError', async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockRejectedValueOnce(new Error('unavailable')).mockResolvedValue(undefined);
    await renderForm({ onSubmit });
    await user.type(titleField(), ' now');
    await user.click(screen.getByRole('button', { name: 'Create task' }));

    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't create the task. Try again.");
    // The form stays open with what the person wrote.
    expect(screen.getByRole('form')).toBeInTheDocument();
    expect(titleField()).toHaveValue('Send the quotation to ACME now');

    // The error goes with the next attempt.
    await user.click(screen.getByRole('button', { name: 'Create task' }));
    expect(onSubmit).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole('alert')).toBeNull();
  });
});

type DateFieldHarnessProps = { lang: UiLang; initial?: string | null; onChange: (value: string | null) => void };

function DateFieldHarness({ lang, initial = null, onChange }: DateFieldHarnessProps) {
  const [value, setValue] = useState(initial);
  return (
    <DateField
      label="Due date"
      lang={lang}
      value={value}
      onChange={(next) => {
        setValue(next);
        onChange(next);
      }}
    />
  );
}

/** The browser's own language, which the field must ignore. */
function browserLanguage(language: string) {
  vi.spyOn(window.navigator, 'language', 'get').mockReturnValue(language);
  vi.spyOn(window.navigator, 'languages', 'get').mockReturnValue([language]);
}

const segmentTypes = () => within(screen.getByRole('group', { name: 'Due date' })).getAllByRole('spinbutton').map((segment) => segment.getAttribute('data-type'));
const segmentText = () => within(screen.getByRole('group', { name: 'Due date' })).getAllByRole('spinbutton').map((segment) => segment.textContent);

describe('DateField', () => {
  it('DateField_FormatsByInterfaceLanguageNotTheBrowserLocale', async () => {
    const user = userEvent.setup();

    // English is day first even in a browser whose own order is year first.
    browserLanguage('ja-JP');
    const english = vi.fn();
    const { unmount } = render(<DateFieldHarness lang="en" onChange={english} />);
    expect(segmentTypes()).toEqual(['day', 'month', 'year']);
    await user.click(within(screen.getByRole('group', { name: 'Due date' })).getAllByRole('spinbutton')[0]);
    await user.keyboard('16102026');
    expect(english).toHaveBeenLastCalledWith('2026-10-16');
    expect(segmentText()).toEqual(['16', '10', '2026']);
    unmount();

    // Thai is day first with a Gregorian year and Latin digits, in a browser whose own order is month first.
    browserLanguage('en-US');
    const thai = vi.fn();
    const filled = render(<DateFieldHarness lang="th" initial="2026-10-16" onChange={thai} />);
    expect(segmentTypes()).toEqual(['day', 'month', 'year']);
    expect(segmentText()).toEqual(['16', '10', '2026']);
    filled.unmount();

    render(<DateFieldHarness lang="th" onChange={thai} />);
    await user.click(within(screen.getByRole('group', { name: 'Due date' })).getAllByRole('spinbutton')[0]);
    await user.keyboard('16102026');
    expect(thai).toHaveBeenLastCalledWith('2026-10-16');
  });

  it('reports null while the date is incomplete, and never a half-typed year', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { unmount } = render(<DateFieldHarness lang="en" onChange={onChange} />);
    const segments = within(screen.getByRole('group', { name: 'Due date' })).getAllByRole('spinbutton');

    // Day and month without a year: nothing is reported but null.
    await user.click(segments[0]);
    await user.keyboard('1610');
    expect(onChange.mock.calls.every(([value]) => value === null)).toBe(true);

    // A year of two digits is not a date yet.
    await user.keyboard('26');
    expect(onChange.mock.calls.every(([value]) => value === null)).toBe(true);
    await user.keyboard('{Backspace}{Backspace}2026');
    expect(onChange).toHaveBeenLastCalledWith('2026-10-16');
    unmount();

    // Clearing a complete date's year takes it back to null, and no half-typed year is reported on the way.
    const cleared = vi.fn();
    render(<DateFieldHarness lang="en" initial="2026-10-16" onChange={cleared} />);
    const year = within(screen.getByRole('group', { name: 'Due date' })).getAllByRole('spinbutton')[2];
    await user.click(year);
    await user.keyboard('{Backspace}{Backspace}{Backspace}{Backspace}');
    expect(cleared).toHaveBeenLastCalledWith(null);
    expect(cleared.mock.calls.every(([value]) => value === null || Number(String(value).slice(0, 4)) >= 1000)).toBe(true);
    // The day and month the person typed stay where they were.
    expect(segmentText().slice(0, 2)).toEqual(['16', '10']);

    // Typing the year again completes it.
    await user.keyboard('2027');
    expect(cleared).toHaveBeenLastCalledWith('2027-10-16');
  });
});

describe('CreatedTaskCard', () => {
  const card = (props: Partial<CreatedTaskCardProps> = {}): ReactElement => (
    <CreatedTaskCard
      ref="TSK-2035"
      title="Send the quotation to ACME"
      ownerName="Nok"
      dueLabel="Fri 16 Oct"
      sourceLabel="From #q4-launch · Arthit K. · 14:02"
      boardHref="/board"
      {...props}
    />
  );

  it('CreatedTaskCard_HasNoPerforationSealOrDraftTag_AndAlwaysRefAndSource', () => {
    const { container, rerender } = render(card());
    const group = screen.getByRole('group', { name: 'Task TSK-2035' });
    expect(group).toHaveAttribute('tabindex', '-1');
    expect(screen.getByRole('heading', { name: 'Send the quotation to ACME' })).toBeInTheDocument();
    expect(screen.getByText('Nok · Fri 16 Oct')).toBeInTheDocument();
    expect(screen.getByText('REF TSK-2035')).toBeInTheDocument();
    expect(screen.getByText('From #q4-launch · Arthit K. · 14:02')).toBeInTheDocument();
    expect(group).toHaveTextContent('Task TSK-2035 created · View on Board');
    expect(within(group).getByRole('link', { name: 'View on Board' })).toHaveAttribute('href', '/board');

    // A plain card: no perforation, no stub-tearing, no seal, no Draft tag and no shadow.
    expect(container.querySelector('.kiwi-perf')).toBeNull();
    expect(screen.queryByRole('img', { name: 'Confirmed' })).toBeNull();
    expect(screen.queryByText('Draft')).toBeNull();
    expect(container.querySelector('.torn')).toBeNull();

    // With no owner and no due date it still carries its REF and its source.
    rerender(card({ ownerName: undefined, dueLabel: undefined, sourceLabel: 'Added by Pimchanok S. · 14:53' }));
    expect(screen.getByText('REF TSK-2035')).toBeInTheDocument();
    expect(screen.getByText('Added by Pimchanok S. · 14:53')).toBeInTheDocument();
  });

  it('CreatedTaskCard_NoOwnerReadsNeutral', () => {
    const { container } = render(card({ ownerName: undefined, dueLabel: undefined }));
    expect(screen.getByText('No owner · No due date')).toBeInTheDocument();
    // The Neutral Rule: no owner is not a warning, so nothing carries the amber look.
    expect(container.innerHTML).not.toMatch(/amber|draft|warn/i);
  });

  it('reads in Thai, with the REF and the task code left literal', () => {
    render(
      <LabelsProvider lang="th">
        {card({ ownerName: undefined, dueLabel: undefined, sourceLabel: 'เพิ่มโดยพิมพ์ชนก ส. · 14:53' })}
      </LabelsProvider>,
    );
    const group = screen.getByRole('group', { name: 'งาน TSK-2035' });
    expect(group).toHaveTextContent('ไม่มีผู้รับผิดชอบ · ไม่มีกำหนดส่ง');
    expect(group).toHaveTextContent('สร้างงาน TSK-2035 แล้ว · ดูบนบอร์ด');
    expect(within(group).getByRole('link', { name: 'ดูบนบอร์ด' })).toHaveAttribute('href', '/board');
    expect(within(group).getByText('REF TSK-2035')).toHaveAttribute('lang', 'en');
    expect(within(group).getByText('TSK-2035')).toHaveAttribute('lang', 'en');
    expectNoEnglish(group);
  });
});

/** The English words of every fixed label: what a Thai screen must never show. */
const ENGLISH_FRAGMENTS = [
  ...new Set(
    Object.values(en)
      .flatMap((value) => (typeof value === 'string' ? [value] : [value.one, value.other]))
      .flatMap((value) => value.split(/\{\w+\}/))
      .map((fragment) => fragment.replace(/[·#,…]/g, ' ').trim())
      .filter((fragment) => /[A-Za-z]{3,}/.test(fragment)),
  ),
];

/** Everything a person or a screen reader can read from the markup: text and every accessible name. */
function expectNoEnglish(root: HTMLElement) {
  const names = [...root.querySelectorAll('[aria-label], [title]')].flatMap((element) => [
    element.getAttribute('aria-label') ?? '',
    element.getAttribute('title') ?? '',
  ]);
  const readable = [root.textContent, ...names].join('\n');
  for (const fragment of ENGLISH_FRAGMENTS) {
    expect(readable, fragment).not.toContain(fragment);
  }
}

describe('under LabelsProvider lang="th"', () => {
  it('the task form reads in Thai, with its Thai due-date field and Thai owner options', async () => {
    const user = userEvent.setup();
    const thaiMembers: Member[] = [{ id: 'u1', name: 'สมชาย ว.' }];
    const { container } = render(
      <LabelsProvider lang="th">
        <TaskForm
          heading="สร้างงานจากข้อความนี้"
          subtitle="จาก #q4-launch · อาทิตย์ ก. · 14:02"
          initialTitle="ส่งใบเสนอราคาให้ ACME"
          channels={[{ id: 'c1', name: 'q4-launch' }]}
          channelId="c1"
          membersFor={() => Promise.resolve(thaiMembers)}
          onSubmit={() => Promise.resolve()}
          onCancel={() => undefined}
        />
      </LabelsProvider>,
    );
    await screen.findByRole('option', { name: 'สมชาย ว.' });
    expect(screen.getByRole('form', { name: 'สร้างงานจากข้อความนี้' })).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'ชื่องาน' })).toHaveValue('ส่งใบเสนอราคาให้ ACME');
    expect(screen.getByRole('combobox', { name: 'แชนเนล' })).toBeInTheDocument();
    expect(within(screen.getByRole('combobox', { name: 'ผู้รับผิดชอบ' })).getByRole('option', { name: 'ไม่มีผู้รับผิดชอบ' })).toBeInTheDocument();
    expect(screen.getByRole('group', { name: 'กำหนดส่ง' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'สร้างงาน' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'ยกเลิก' })).toBeInTheDocument();

    await user.clear(screen.getByRole('textbox', { name: 'ชื่องาน' }));
    await user.click(screen.getByRole('button', { name: 'สร้างงาน' }));
    expect(screen.getByText('ชื่องานต้องไม่ว่าง')).toHaveAttribute('lang', 'th');
    expectNoEnglish(container);
  });
});
