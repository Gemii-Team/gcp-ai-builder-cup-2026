// @vitest-environment jsdom
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useRef, useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { Button } from './Button';
import { dateToDueAt, dueAtToDate, formatClock, formatDay, formatOffset, formatSize } from './format';
import { Icon, type IconName } from './icons';
import { IconButton } from './IconButton';
import { EnMarker, LangRuns, Rich, langRuns } from './LangRuns';
import { Mark, MarkSmall, Mascot, Seal } from './marks';
import { Menu, MenuItem, MenuRadioItem } from './Menu';
import { Segmented } from './Segmented';

describe('formatOffset: a transcript offset is h:mm:ss with no leading hour zero', () => {
  it.each([
    [0, '0:00:00'],
    [761000, '0:12:41'],
    [3725000, '1:02:05'],
    [36000000, '10:00:00'],
  ])('%i ms is %s', (ms, expected) => {
    expect(formatOffset(ms)).toBe(expected);
  });

  it('is null for a null, undefined or negative offset, never a false 0:00:00', () => {
    expect(formatOffset(null)).toBeNull();
    expect(formatOffset(undefined)).toBeNull();
    expect(formatOffset(-1)).toBeNull();
  });
});

describe('formatClock: a wall-clock time is hh:mm on a 24-hour clock in Asia/Bangkok', () => {
  it('formats 07:31Z as 14:31, never in the offset shape', () => {
    const clock = formatClock('2026-10-11T07:31:00Z');
    expect(clock).toBe('14:31');
    expect(clock).not.toMatch(/^\d:\d{2}:\d{2}$/);
  });

  it('writes the hour after midnight as 00, not 24', () => {
    expect(formatClock(new Date('2026-10-10T17:05:00Z'))).toBe('00:05');
  });

  it('honours an explicit time zone', () => {
    expect(formatClock('2026-10-11T07:31:00Z', 'UTC')).toBe('07:31');
  });
});

describe('formatSize', () => {
  it.each([
    [14 * 1024, '14 KB'],
    [100, '1 KB'],
    [2 * 1048576, '2.0 MB'],
    [1.5 * 1048576, '1.5 MB'],
  ])('%i bytes is %s', (bytes, expected) => {
    expect(formatSize(bytes)).toBe(expected);
  });
});

describe('formatDay: the day label of a due date, a done date or a message day', () => {
  it('writes Fri 16 Oct for a due date in English', () => {
    expect(formatDay('2026-10-16', 'en')).toBe('Fri 16 Oct');
  });

  it('keeps the calendar day of a date-only string in any zone', () => {
    expect(formatDay('2026-10-16', 'en', 'America/Los_Angeles')).toBe('Fri 16 Oct');
  });

  it('shows the Bangkok day of an instant, which is the next day just after 17:00Z', () => {
    expect(formatDay('2026-10-15T17:30:00Z', 'en')).toBe('Fri 16 Oct');
    expect(formatDay(new Date('2026-10-15T16:30:00Z'), 'en')).toBe('Thu 15 Oct');
  });

  it('writes the Thai short weekday and month', () => {
    expect(formatDay('2026-10-16', 'th')).toBe('ศ. 16 ต.ค.');
  });
});

describe('dateToDueAt and dueAtToDate: a due date is midnight in Asia/Bangkok', () => {
  it('round-trips 2026-10-16 through 2026-10-15T17:00:00Z', () => {
    const due = dateToDueAt('2026-10-16');
    expect(due.toISOString()).toBe('2026-10-15T17:00:00.000Z');
    expect(dueAtToDate(due)).toBe('2026-10-16');
  });

  it('is never off by one around midnight Bangkok', () => {
    expect(dueAtToDate(new Date('2026-10-15T16:59:59Z'))).toBe('2026-10-15');
    expect(dueAtToDate(new Date('2026-10-15T17:00:00Z'))).toBe('2026-10-16');
    expect(dueAtToDate(new Date('2026-10-16T16:59:59Z'))).toBe('2026-10-16');
    expect(dueAtToDate(new Date('2026-10-16T17:00:00Z'))).toBe('2026-10-17');
  });

  it('round-trips the ends of a month and a year', () => {
    for (const iso of ['2026-10-31', '2026-12-31', '2027-01-01', '2028-02-29']) {
      expect(dueAtToDate(dateToDueAt(iso))).toBe(iso);
    }
  });
});

describe('LangRuns: the Thai Tag Rule', () => {
  it('wraps a Thai run, with the Latin words between its Thai characters, in one lang="th" span', () => {
    const { container } = render(<LangRuns text="ตรวจสอบ Q4 launch เสร็จ" />);
    const spans = container.querySelectorAll('span[lang]');
    expect(spans).toHaveLength(1);
    expect(spans[0]).toHaveAttribute('lang', 'th');
    expect(spans[0]).toHaveTextContent('ตรวจสอบ Q4 launch เสร็จ');
  });

  it('leaves English text alone: no span, the inherited language stays', () => {
    const { container } = render(<LangRuns text="Move the launch to 15 Nov 2026" />);
    expect(container.querySelector('[lang]')).toBeNull();
    expect(container).toHaveTextContent('Move the launch to 15 Nov 2026');
  });

  it('tags each Thai run of a mixed sentence and keeps the English around it', () => {
    const { container } = render(<LangRuns text="Task: ส่งใบเสนอราคา — due Friday, อาทิตย์ ก. ยืนยัน" />);
    const runs = [...container.querySelectorAll('span[lang="th"]')].map((span) => span.textContent);
    expect(runs).toEqual(['ส่งใบเสนอราคา', 'อาทิตย์ ก. ยืนยัน']);
    expect(container).toHaveTextContent('Task: ส่งใบเสนอราคา — due Friday, อาทิตย์ ก. ยืนยัน');
  });

  it('returns plain text for empty input', () => {
    expect(langRuns('')).toEqual([]);
  });

  it('marks a Latin marker inside Thai prose lang="en" through EnMarker', () => {
    render(
      <p lang="th">
        สัญญาสิ้นสุดวันที่ 31 ธ.ค. <EnMarker>[1]</EnMarker>
      </p>,
    );
    expect(screen.getByText('[1]')).toHaveAttribute('lang', 'en');
  });
});

describe('Rich: a Thai string that marks its Latin words as <en>', () => {
  it('renders each <en> word as one lang="en" span and everything else as plain text', () => {
    const { container } = render(<Rich text="ดำเนินการต่อด้วย <en>Google</en>" />);
    const spans = container.querySelectorAll('span[lang]');
    expect(spans).toHaveLength(1);
    expect(spans[0]).toHaveAttribute('lang', 'en');
    expect(spans[0]).toHaveTextContent('Google');
    expect(container).toHaveTextContent('ดำเนินการต่อด้วย Google');
  });

  it('handles several markers and never interprets other markup', () => {
    const { container } = render(<Rich text="<en>K.I.W.I.</en> จะเห็น <en>Drive</en> และ <b>ไม่</b>" />);
    expect([...container.querySelectorAll('span[lang="en"]')].map((span) => span.textContent)).toEqual([
      'K.I.W.I.',
      'Drive',
    ]);
    expect(container.querySelector('b')).toBeNull();
    expect(container).toHaveTextContent('K.I.W.I. จะเห็น Drive และ <b>ไม่</b>');
  });
});

describe('Button', () => {
  it('exposes the reason a disabled button is disabled through aria-describedby', () => {
    render(
      <Button variant="primary" size="sm" disabled disabledReason="Choose an owner first.">
        Confirm
      </Button>,
    );
    const button = screen.getByRole('button', { name: 'Confirm' });
    expect(button).toBeDisabled();
    const describedBy = button.getAttribute('aria-describedby');
    expect(describedBy).toBeTruthy();
    expect(document.getElementById(describedBy ?? '')).toHaveTextContent('Choose an owner first.');
  });

  it('carries no description while it is enabled', () => {
    render(<Button disabledReason="Choose an owner first.">Confirm</Button>);
    expect(screen.getByRole('button', { name: 'Confirm' })).not.toHaveAttribute('aria-describedby');
  });

  it('is a button that does not submit forms unless asked to, and fires onClick', async () => {
    const onClick = vi.fn();
    render(<Button onClick={onClick}>Edit</Button>);
    const button = screen.getByRole('button', { name: 'Edit' });
    expect(button).toHaveAttribute('type', 'button');
    await userEvent.click(button);
    expect(onClick).toHaveBeenCalledTimes(1);
  });
});

describe('IconButton', () => {
  it('is named by its label and carries its glyph', () => {
    render(
      <IconButton label="Close">
        <Icon name="close" />
      </IconButton>,
    );
    expect(screen.getByRole('button', { name: 'Close' })).toBeInTheDocument();
  });
});

describe('Icon', () => {
  const NAMES: IconName[] = [
    'ask', 'video', 'board', 'chat', 'user', 'file', 'warning', 'clock', 'send', 'back', 'close',
    'chevron-up', 'chevron-down', 'paperclip', 'signout', 'check', 'filter', 'search', 'search-none',
    'circle', 'arrow-right', 'task-add', 'pen',
  ];

  it.each(NAMES)('draws %s as an 18px stroked line icon in currentColor', (name) => {
    const { container } = render(<Icon name={name} />);
    const svg = container.querySelector('svg');
    expect(svg).not.toBeNull();
    expect(svg).toHaveAttribute('width', '18');
    expect(svg).toHaveAttribute('stroke', 'currentColor');
    expect(svg).toHaveAttribute('stroke-width', '1.75');
    expect(svg?.querySelector('path, circle, rect')).not.toBeNull();
  });
});

describe('brand marks: the Mark Size Rule', () => {
  it('rejects the ringed mark below 40px at compile time', () => {
    // @ts-expect-error the Mark Size Rule: the ringed mark is 40, 48 or 64; 24 belongs to MarkSmall
    render(<Mark size={24} />);
  });

  it('rejects the plain mark above 24px at compile time', () => {
    // @ts-expect-error the Mark Size Rule: the plain mark is 16, 20 or 24; 40 belongs to Mark
    render(<MarkSmall size={40} />);
  });

  it('draws the marks with token classes, never literal hex', () => {
    const { container } = render(
      <>
        <Mark size={40} />
        <MarkSmall size={16} />
        <Seal />
        <Mascot variant="plain" size={112} />
        <Mascot variant="sealed" size={176} />
      </>,
    );
    expect(container.innerHTML).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
    expect(container.querySelector('.fill-primary')).not.toBeNull();
  });

  it('labels a mark only when asked to, otherwise it is decorative', () => {
    const { container } = render(
      <>
        <MarkSmall size={16} label="Summarized by K.I.W.I." />
        <MarkSmall size={16} />
      </>,
    );
    expect(screen.getByRole('img', { name: 'Summarized by K.I.W.I.' })).toBeInTheDocument();
    expect(container.querySelectorAll('svg[aria-hidden="true"]')).toHaveLength(1);
  });

  it('stamps the seal CONFIRMED, rotated -8 degrees', () => {
    render(<Seal />);
    const seal = screen.getByRole('img', { name: 'Confirmed' });
    expect(seal).toHaveAttribute('width', '80');
    expect(seal).toHaveClass('[transform:rotate(-8deg)]');
    expect(seal).toHaveTextContent('CONFIRMED');
  });

  it('sizes the sealed mascot from its 176 by 104 drawing', () => {
    const { container } = render(<Mascot variant="sealed" size={132} />);
    const svg = container.querySelector('svg');
    expect(svg).toHaveAttribute('width', '132');
    expect(svg).toHaveAttribute('height', String((132 * 104) / 176));
    expect(svg).toHaveAttribute('aria-hidden', 'true');
  });
});

function MenuHarness() {
  const [open, setOpen] = useState(false);
  const [picked, setPicked] = useState('none');
  const anchor = useRef<HTMLButtonElement>(null);
  return (
    <>
      <button type="button" ref={anchor} onClick={() => setOpen(true)}>
        Move
      </button>
      <p>picked: {picked}</p>
      <Menu label="Move to" open={open} onClose={() => setOpen(false)} anchorRef={anchor} placement="below">
        <MenuRadioItem checked={picked === 'todo'} onSelect={() => setPicked('todo')} swatch="bg-perforation-sage">
          To do
        </MenuRadioItem>
        <MenuRadioItem checked={picked === 'doing'} onSelect={() => setPicked('doing')} swatch="bg-doing-teal">
          Doing
        </MenuRadioItem>
        <MenuRadioItem checked={picked === 'done'} onSelect={() => setPicked('done')} swatch="bg-seal-green">
          Done
        </MenuRadioItem>
      </Menu>
    </>
  );
}

describe('Menu', () => {
  it('stays closed until opened and then focuses its first row', async () => {
    const user = userEvent.setup();
    render(<MenuHarness />);
    expect(screen.queryByRole('menu')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Move' }));
    expect(screen.getByRole('menu', { name: 'Move to' })).toBeInTheDocument();
    expect(screen.getByRole('menuitemradio', { name: 'To do' })).toHaveFocus();
  });

  it('moves focus with the arrow keys (wrapping), Home and End', async () => {
    const user = userEvent.setup();
    render(<MenuHarness />);
    await user.click(screen.getByRole('button', { name: 'Move' }));
    const [todo, doing, done] = screen.getAllByRole('menuitemradio');
    await user.keyboard('{ArrowDown}');
    expect(doing).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(done).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(todo).toHaveFocus();
    await user.keyboard('{ArrowUp}');
    expect(done).toHaveFocus();
    await user.keyboard('{Home}');
    expect(todo).toHaveFocus();
    await user.keyboard('{End}');
    expect(done).toHaveFocus();
  });

  it('closes on Escape and returns focus to the opener', async () => {
    const user = userEvent.setup();
    render(<MenuHarness />);
    const opener = screen.getByRole('button', { name: 'Move' });
    await user.click(opener);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('menu')).toBeNull();
    expect(opener).toHaveFocus();
  });

  it('closes on Tab without trapping focus', async () => {
    const user = userEvent.setup();
    render(<MenuHarness />);
    await user.click(screen.getByRole('button', { name: 'Move' }));
    await user.keyboard('{Tab}');
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('exposes the current choice through aria-checked and selects on press', async () => {
    const user = userEvent.setup();
    render(<MenuHarness />);
    await user.click(screen.getByRole('button', { name: 'Move' }));
    for (const row of screen.getAllByRole('menuitemradio')) {
      expect(row).toHaveAttribute('aria-checked', 'false');
    }
    await user.click(screen.getByRole('menuitemradio', { name: 'Doing' }));
    expect(screen.getByText('picked: doing')).toBeInTheDocument();
    expect(screen.queryByRole('menu')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Move' }));
    expect(screen.getByRole('menuitemradio', { name: 'Doing' })).toHaveAttribute('aria-checked', 'true');
    expect(screen.getByRole('menuitemradio', { name: 'Doing' })).toHaveFocus();
  });

  it('closes when the window is resized, because the menu is placed once from its opener', async () => {
    const user = userEvent.setup();
    render(<MenuHarness />);
    await user.click(screen.getByRole('button', { name: 'Move' }));
    window.dispatchEvent(new Event('resize'));
    await vi.waitFor(() => {
      expect(screen.queryByRole('menu')).toBeNull();
    });
  });

  it('renders a link row for an href and a button row for onSelect, each with its icon', async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    function Account() {
      const anchor = useRef<HTMLButtonElement>(null);
      return (
        <>
          <button type="button" ref={anchor}>
            Account
          </button>
          <Menu label="Account" open onClose={() => undefined} anchorRef={anchor} placement="above">
            <MenuItem icon="signout" href="/signin#signedout" onSelect={onSelect}>
              Sign out
            </MenuItem>
            <MenuItem icon="user" onSelect={onSelect}>
              Profile
            </MenuItem>
          </Menu>
        </>
      );
    }
    render(<Account />);
    const link = within(screen.getByRole('menu')).getByRole('menuitem', { name: 'Sign out' });
    expect(link).toHaveAttribute('href', '/signin#signedout');
    expect(link.querySelector('svg')).not.toBeNull();
    const row = within(screen.getByRole('menu')).getByRole('menuitem', { name: 'Profile' });
    expect(row.tagName).toBe('BUTTON');
    await user.click(row);
    expect(onSelect).toHaveBeenCalledTimes(1);
  });
});

describe('Segmented', () => {
  const options = [
    { value: 'en', label: 'English', lang: 'en' },
    { value: 'th', label: 'ไทย', lang: 'th' },
  ];

  it('follows value with aria-pressed and reports the chosen option', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { rerender } = render(<Segmented label="Language" value="en" onChange={onChange} options={options} />);
    expect(screen.getByRole('group', { name: 'Language' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'English' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('button', { name: 'ไทย' })).toHaveAttribute('aria-pressed', 'false');
    await user.click(screen.getByRole('button', { name: 'ไทย' }));
    expect(onChange).toHaveBeenCalledWith('th');
    rerender(<Segmented label="Language" value="th" onChange={onChange} options={options} />);
    expect(screen.getByRole('button', { name: 'English' })).toHaveAttribute('aria-pressed', 'false');
    expect(screen.getByRole('button', { name: 'ไทย' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('keeps the language of an option that names itself in its own language', () => {
    render(<Segmented label="Language" value="en" onChange={() => undefined} options={options} />);
    expect(screen.getByRole('button', { name: 'ไทย' })).toHaveAttribute('lang', 'th');
    expect(screen.getByRole('button', { name: 'English' })).toHaveAttribute('lang', 'en');
  });

  it('sets a count in mono beside the label', () => {
    render(
      <Segmented
        variant="status"
        label="Status"
        value="todo"
        onChange={() => undefined}
        options={[
          { value: 'todo', label: 'To do', count: 7 },
          { value: 'doing', label: 'Doing', count: 4 },
        ]}
      />,
    );
    const button = screen.getByRole('button', { name: 'To do 7' });
    expect(within(button).getByText('7')).toHaveClass('font-mono');
  });
});
