import { describe, expect, it } from 'vitest';
import { mimeOf } from './mime';

describe('mimeOf', () => {
  it.each([
    ['report.pdf', 'application/pdf'],
    ['scan.png', 'image/png'],
    ['photo.jpg', 'image/jpeg'],
    ['photo.jpeg', 'image/jpeg'],
    ['notes.txt', 'text/plain'],
    ['readme.md', 'text/markdown'],
    ['table.csv', 'text/csv'],
    ['standup.wav', 'audio/wav'],
  ])('helps a browser that gave none for %s', (name, mime) => {
    expect(mimeOf({ name, type: '' })).toBe(mime);
  });

  it('reads the extension in any case, and only the last one', () => {
    expect(mimeOf({ name: 'REPORT.PDF', type: '' })).toBe('application/pdf');
    expect(mimeOf({ name: 'archive.pdf.zip', type: '' })).toBe('');
  });

  it('does not guess for any other file: it is sent as the browser gave it', () => {
    for (const name of ['notes.docx', 'page.html', 'logo.svg', 'song.mp3', 'clip.mp4', 'noextension', '.md', 'constructor', 'toString']) {
      expect(mimeOf({ name, type: '' }), name).toBe('');
    }
  });

  it('keeps the type the browser gave, whatever the extension says', () => {
    expect(mimeOf({ name: 'notes.md', type: 'text/plain' })).toBe('text/plain');
    expect(mimeOf({ name: 'song.mp3', type: 'audio/mpeg' })).toBe('audio/mpeg');
  });
});
