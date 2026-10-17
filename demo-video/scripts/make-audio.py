"""Synthesizes the demo's audio from scratch (no samples, no licences): a quiet ambient music bed that loops, and three
soft effects. Run with `python3 -I scripts/make-audio.py`; writes public/audio/*.wav (44.1 kHz, 16-bit mono)."""
import math, random, struct, wave, os

RATE = 44100
OUT = os.path.join(os.path.dirname(__file__), '..', 'public', 'audio')
random.seed(7)

def write(name, samples, peak_db):
    peak = max(1e-9, max(abs(s) for s in samples))
    gain = (10 ** (peak_db / 20)) / peak
    with wave.open(os.path.join(OUT, name), 'wb') as w:
        w.setnchannels(1); w.setsampwidth(2); w.setframerate(RATE)
        w.writeframes(b''.join(struct.pack('<h', int(max(-1, min(1, s * gain)) * 32767)) for s in samples))
    print(f'{name}: {len(samples) / RATE:.2f}s')

def lowpass(samples, cutoff):
    a = 1 - math.exp(-2 * math.pi * cutoff / RATE); y = 0.0; out = []
    for s in samples:
        y += a * (s - y); out.append(y)
    return out

def hz(note):  # MIDI note number to frequency
    return 440 * 2 ** ((note - 69) / 12)

# --- Music bed: four chords of 8 s (Am9, Fmaj7, Cmaj7, G6), slow-swelling pads and a soft arpeggio; loops seamlessly.
CHORDS = [[57, 60, 64, 67, 71], [53, 57, 60, 64, 69], [48, 55, 59, 64, 67], [55, 59, 62, 64, 71]]
BAR = 8.0
total = int(BAR * len(CHORDS) * RATE)
music = [0.0] * total
for ci, chord in enumerate(CHORDS):
    start = int(ci * BAR * RATE)
    length = int(BAR * RATE)
    for note in chord:
        for detune in (-0.12, 0.12):
            f = hz(note) * 2 ** (detune / 12); ph = random.random() * 6.28
            for i in range(length + int(1.5 * RATE)):  # tails overlap into the next chord for a smooth change
                t = i / RATE
                env = min(1.0, t / 1.6) * (1.0 if t < BAR else max(0.0, 1 - (t - BAR) / 1.5))
                env *= 0.85 + 0.15 * math.sin(2 * math.pi * 0.17 * t + note)
                music[(start + i) % total] += 0.05 * env * math.sin(2 * math.pi * f * t + ph)
    # arpeggio: eighth notes at 88 bpm, an octave up, very soft plucks
    step = 60 / 88 / 2
    for k in range(int(BAR / step)):
        note = chord[[0, 2, 4, 2, 1, 3, 4, 3][k % 8]] + 12
        f = hz(note); s0 = start + int(k * step * RATE)
        for i in range(int(0.9 * RATE)):
            t = i / RATE
            music[(s0 + i) % total] += 0.035 * math.exp(-t * 5) * (math.sin(2 * math.pi * f * t) + 0.3 * math.sin(4 * math.pi * f * t))
music = lowpass(music, 2600)
write('music.wav', music, -14)

# --- Click: a soft, short tick for presses.
click = [math.exp(-i / RATE * 140) * (0.7 * math.sin(2 * math.pi * 1900 * i / RATE) + 0.3 * (random.random() * 2 - 1)) for i in range(int(0.05 * RATE))]
write('click.wav', lowpass(click, 6000), -6)

# --- Whoosh: filtered noise that swells and falls, for scene changes.
n = int(0.7 * RATE); whoosh = []; y = 0.0
for i in range(n):
    t = i / n; cutoff = 300 + 2600 * math.sin(math.pi * t)
    a = 1 - math.exp(-2 * math.pi * cutoff / RATE); y += a * ((random.random() * 2 - 1) - y)
    whoosh.append(y * math.sin(math.pi * t) ** 2)
write('whoosh.wav', whoosh, -6)

# --- Chime: a two-note bell for an AI result arriving (the minutes, an answer, a confirmed task).
chime = [0.0] * int(1.8 * RATE)
for offset, note in ((0.0, 84), (0.12, 91)):
    s0 = int(offset * RATE); f = hz(note)
    for i in range(len(chime) - s0):
        t = i / RATE; env = math.exp(-t * 3.2)
        chime[s0 + i] += env * (math.sin(2 * math.pi * f * t) + 0.4 * math.sin(2 * math.pi * 2.76 * f * t) * math.exp(-t * 6))
write('chime.wav', chime, -6)
