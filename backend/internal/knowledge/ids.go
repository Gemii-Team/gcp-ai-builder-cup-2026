package knowledge

import (
	"strconv"

	"github.com/google/uuid"
)

// ChunkID is the id of the chunk at anchor of a source: UUIDv5 over
// "kind/sourceID/anchor" in the namespace of the workspace. The same text of the same
// source always lands on the same row, which is what makes indexing idempotent, and
// no two workspaces can produce the same id.
func ChunkID(ws uuid.UUID, kind string, sourceID uuid.UUID, anchor string) uuid.UUID {
	return uuid.NewSHA1(ws, []byte(kind+"/"+sourceID.String()+"/"+anchor))
}

// pieceID is the id of piece n of the chunk text at anchor. A line or a page too long
// for one chunk is cut into several, all at the same anchor, so that a citation
// still points at the right place; piece 0 takes the anchor's own id and piece n the
// id of "anchor#n".
func pieceID(ws uuid.UUID, kind string, sourceID uuid.UUID, anchor string, piece int) uuid.UUID {
	if piece > 0 {
		anchor += "#" + strconv.Itoa(piece)
	}
	return ChunkID(ws, kind, sourceID, anchor)
}
