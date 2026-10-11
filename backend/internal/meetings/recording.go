package meetings

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	eventsv1 "kiwi/gen/kiwi/events/v1"
	filesv1 "kiwi/gen/kiwi/files/v1"
	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
	"kiwi/internal/meetings/internal/store"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/llm"
	"kiwi/internal/platform/outbox"
)

const (
	consumerTranscribe = "meeting.transcribe"
	// maxRecordingBytes is the largest recording that is read: the 200 MiB an upload may
	// have.
	maxRecordingBytes = 200 << 20
)

// FileInfo is what CreateFromRecording needs to know about an uploaded file.
type FileInfo struct {
	WorkspaceID uuid.UUID
	ChannelID   uuid.UUID
	Purpose     filesv1.UploadPurpose
	Status      filesv1.FileStatus
}

// FileChecker describes file fileID as a may read it: the docs domain looks it up, and
// answers errs.ErrNotFound for a file that is not in the actor's workspace and
// errs.ErrForbidden when the actor may not read it. The meetings domain never reads the
// files schema itself.
type FileChecker func(ctx context.Context, a authz.Actor, fileID uuid.UUID) (FileInfo, error)

// checkRecordingFile reads the file a recording meeting would be made from and accepts
// it only when it is an uploaded recording of this workspace and channel that a may
// use.
func (s *service) checkRecordingFile(ctx context.Context, a authz.Actor, channelID, fileID uuid.UUID) error {
	if s.files == nil {
		return errors.New("meetings: no file checker is configured to verify a recording")
	}
	info, err := s.files(ctx, a, fileID)
	if err != nil {
		return err
	}
	switch {
	case info.WorkspaceID != a.WorkspaceID:
		return fmt.Errorf("meetings: file %s is not in the actor's workspace: %w", fileID, errs.ErrForbidden)
	case info.Purpose != filesv1.UploadPurpose_UPLOAD_PURPOSE_RECORDING:
		return fmt.Errorf("meetings: file %s is not a recording: %w", fileID, errs.ErrInvalid)
	case info.ChannelID != channelID:
		return fmt.Errorf("meetings: recording %s was uploaded to another channel: %w", fileID, errs.ErrInvalid)
	case info.Status != filesv1.FileStatus_FILE_STATUS_UPLOADED && info.Status != filesv1.FileStatus_FILE_STATUS_READY:
		return fmt.Errorf("meetings: recording %s is %s, not uploaded: %w", fileID, info.Status, errs.ErrInvalid)
	}
	return nil
}

// recordingKey is where docs keeps the bytes of file id of workspace ws: the one
// convention the two domains share, since only docs writes the object and only the
// transcribe job reads it.
func recordingKey(ws, id uuid.UUID) string { return "uploads/" + ws.String() + "/" + id.String() }

// permanent is a failure that retrying cannot cure: the meeting ends failed with reason
// and the message is acknowledged.
type permanent struct {
	reason string
	err    error
}

func (p *permanent) Error() string { return p.reason + ": " + p.err.Error() }
func (p *permanent) Unwrap() error { return p.err }

// prepareTranscribe is the meeting.transcribe job. It turns the recording of a meeting
// into the transcript entries of a meeting and then goes on as meeting.ingest does:
// summarize, and one transaction that writes the lot. It is a long job (the model takes
// about a fifth of the recording's length) and holds no transaction while it works: the
// meeting is ingesting for the whole time, and a process that stops in the middle leaves
// the message with the outbox to be delivered again, which starts over on the meeting
// as it is. The recording is the file the meeting row names, never the one in the
// message: the message only says which meeting to work on.
//
// The recording is checked again here, on the bytes about to be sent. Upload checked it
// once, but a signed upload URL takes more than one PUT, and a body the transcription
// model cannot decode would stop it for everyone for a minute. A recording that is gone
// or no longer a PCM 16-bit WAV, one with no speech in it, and a transcript too long for
// the summary end the meeting failed with a reason; a model that keeps failing does so on
// the final attempt; anything else leaves the message to be delivered again.
func (p *pipeline) prepareTranscribe(ctx context.Context, m bus.Message) (outbox.Apply, error) {
	var ev eventsv1.MeetingTranscribe
	if err := decodeEvent(m, &ev); err != nil {
		return nil, err
	}
	ws, err := parseID(ev.GetWorkspaceId(), "workspace id")
	if err != nil {
		return nil, err
	}
	id, err := parseID(ev.GetMeetingId(), "meeting id")
	if err != nil {
		return nil, err
	}
	row, err := load(ctx, p.db, ws, id)
	if errors.Is(err, errs.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row, ok, err := p.startTranscribing(ctx, row)
	if err != nil || !ok {
		return nil, err
	}

	data, err := p.readRecording(ctx, row)
	var perm *permanent
	if errors.As(err, &perm) {
		return p.fail(ctx, row, perm.reason, m.Attempt, err), nil
	}
	if err != nil {
		return nil, err
	}

	bus.KeepAlive(ctx)
	read, err := p.llm.Transcribe(ctx, llm.Audio{Data: data, MIME: "audio/wav", Name: "recording.wav"})
	if err != nil {
		return p.modelFailed(ctx, row, m, err)
	}
	entries := transcriptEntries(read)
	if len(entries) == 0 {
		return p.fail(ctx, row, reasonNoSpeech, m.Attempt, errors.New("the recording has no speech in it")), nil
	}
	return p.summarize(ctx, row, m, entries)
}

// startTranscribing moves a pending recording meeting to ingesting, announces it, and
// returns it. The second result is false when there is nothing for the message to do:
// the meeting is gone, is not a recording, or has settled (the event a retry leaves
// behind, or a message delivered again after the meeting finished). A meeting that is
// already ingesting is one whose job was interrupted, and goes on.
func (p *pipeline) startTranscribing(ctx context.Context, row store.MeetingsMeeting) (store.MeetingsMeeting, bool, error) {
	if row.RecordingFileID == nil || (row.Status != statusPending && row.Status != statusIngesting) {
		return row, false, nil
	}
	if row.Status == statusIngesting {
		return row, true, nil
	}
	ok := false
	err := p.db.InWorkspace(ctx, row.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.LockMeeting(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("meetings: lock meeting: %w", err)
		}
		switch cur.Status {
		case statusIngesting:
			row, ok = cur, true
			return nil
		case statusPending:
		default:
			return nil
		}
		started, err := q.StartIngest(ctx, cur.ID)
		if err != nil {
			return fmt.Errorf("meetings: start transcribing: %w", err)
		}
		row, ok = started, true
		return enqueueStatus(ctx, tx, started)
	})
	if err != nil {
		return row, false, err
	}
	return row, ok, nil
}

// readRecording reads the recording of row and checks it is a PCM 16-bit WAV. A
// recording that is gone, too big or not a WAV is a *permanent failure; a store that
// answers with an error may do better another time.
func (p *pipeline) readRecording(ctx context.Context, row store.MeetingsMeeting) ([]byte, error) {
	key := recordingKey(row.WorkspaceID, *row.RecordingFileID)
	attrs, err := p.blob.Head(ctx, key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &permanent{reasonUnreadable, fmt.Errorf("meetings: recording %s is gone: %w", *row.RecordingFileID, err)}
	}
	if err != nil {
		return nil, fmt.Errorf("meetings: look up recording %s: %w", *row.RecordingFileID, err)
	}
	if attrs.Size > maxRecordingBytes {
		return nil, &permanent{reasonUnsupportedAudio, fmt.Errorf("meetings: recording %s is %d bytes, over the %d it may have", *row.RecordingFileID, attrs.Size, int64(maxRecordingBytes))}
	}
	bus.KeepAlive(ctx)
	r, err := p.blob.Read(ctx, key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &permanent{reasonUnreadable, fmt.Errorf("meetings: recording %s is gone: %w", *row.RecordingFileID, err)}
	}
	if err != nil {
		return nil, fmt.Errorf("meetings: read recording %s: %w", *row.RecordingFileID, err)
	}
	defer func() { _ = r.Close() }()
	var buf bytes.Buffer
	buf.Grow(int(attrs.Size) + 1)
	if _, err := io.Copy(&buf, io.LimitReader(r, maxRecordingBytes+1)); err != nil {
		return nil, fmt.Errorf("meetings: read recording %s: %w", *row.RecordingFileID, err)
	}
	if buf.Len() > maxRecordingBytes {
		return nil, &permanent{reasonUnsupportedAudio, fmt.Errorf("meetings: recording %s is over the %d bytes it may have", *row.RecordingFileID, int64(maxRecordingBytes))}
	}
	if err := checkWAV(buf.Bytes()); err != nil {
		return nil, &permanent{reasonUnsupportedAudio, fmt.Errorf("meetings: recording %s: %w", *row.RecordingFileID, err)}
	}
	return buf.Bytes(), nil
}

// transcriptEntries writes what the model heard as the entries of a transcript: in
// order, numbered from 0, with the text cleaned and a sentence that came out empty
// left out. Speaker and timing stay unset when the model gave none, which is what the
// local model does.
func transcriptEntries(read []llm.TranscriptEntry) []*meetingsv1.Entry {
	entries := make([]*meetingsv1.Entry, 0, len(read))
	for _, e := range read {
		text := strings.TrimSpace(cleanText(e.Text))
		if text == "" {
			continue
		}
		out := &meetingsv1.Entry{Idx: int32(len(entries)), Text: text, StartMs: e.StartMS, EndMs: e.EndMS}
		if speaker := strings.TrimSpace(cleanText(e.Speaker)); speaker != "" {
			out.Speaker = proto.String(speaker)
		}
		entries = append(entries, out)
	}
	return entries
}

// errNotWAV is why checkWAV refused a recording.
var errNotWAV = errors.New("not a PCM 16-bit WAV file with audio in it")

// checkWAV accepts only what the transcription model can decode: a RIFF/WAVE file whose
// fmt chunk says PCM, 16 bits per sample, mono or stereo, at any sample rate, followed by
// a data chunk that holds at least one frame of audio. It is the check upload makes, made
// again on the bytes that are about to be sent. A data chunk whose size field is 0 or
// 0xFFFFFFFF, as streaming writers leave it, is as long as the file is.
func checkWAV(data []byte) error {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return fmt.Errorf("%w: no RIFF/WAVE header", errNotWAV)
	}
	frame := int64(0) // bytes of one sample frame, once the fmt chunk is read
	for off := int64(12); off+8 <= int64(len(data)); {
		id, size := string(data[off:off+4]), int64(binary.LittleEndian.Uint32(data[off+4:off+8]))
		body := off + 8
		switch id {
		case "fmt ":
			if size < 16 || body+16 > int64(len(data)) {
				return fmt.Errorf("%w: truncated fmt chunk", errNotWAV)
			}
			format := binary.LittleEndian.Uint16(data[body:])
			channels := binary.LittleEndian.Uint16(data[body+2:])
			rate := binary.LittleEndian.Uint32(data[body+4:])
			bits := binary.LittleEndian.Uint16(data[body+14:])
			if format != 1 || (channels != 1 && channels != 2) || bits != 16 || rate == 0 {
				return fmt.Errorf("%w: format %d, %d channels, %d bits, %d Hz", errNotWAV, format, channels, bits, rate)
			}
			frame = int64(channels) * 2
		case "data":
			if frame == 0 {
				return fmt.Errorf("%w: the data chunk comes before the fmt chunk", errNotWAV)
			}
			audio := int64(len(data)) - body
			if size != 0 && size != 0xFFFFFFFF {
				audio = min(size, audio)
			}
			if audio < frame {
				return fmt.Errorf("%w: the data chunk is empty", errNotWAV)
			}
			return nil
		}
		// Chunks are padded to an even size. size is untrusted: a chunk that runs past
		// the end of the file ends the scan.
		next := body + size + size&1
		if next > int64(len(data)) {
			break
		}
		off = next
	}
	if frame == 0 {
		return fmt.Errorf("%w: no fmt chunk", errNotWAV)
	}
	return fmt.Errorf("%w: no data chunk", errNotWAV)
}
