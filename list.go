package riff

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	// ListID is the identifier of a RIFF LIST chunk, a container holding
	// sub chunks of its own (metadata such as the title or artist of an
	// old recording lives here, invisible to a plain top level scan).
	ListID = [4]byte{'L', 'I', 'S', 'T'}
	// InfoListID is the list type of a LIST chunk holding INFO metadata.
	InfoListID = [4]byte{'I', 'N', 'F', 'O'}

	// ErrListTruncated is reported through ListIterator.Err when the
	// declared size of a LIST chunk, or of one of its sub chunks, runs
	// past the boundary of the data available inside its parent.
	ErrListTruncated = errors.New("riff: declared LIST size exceeds available data")
	// ErrListTrailingData is reported when a LIST boundary leaves behind
	// fewer bytes than a sub chunk header (8 bytes).
	ErrListTrailingData = errors.New("riff: trailing bytes too small for a sub chunk header")
)

// ListSubChunk is a single sub chunk inside a LIST chunk.
type ListSubChunk struct {
	// ID is the 4-byte identifier of the sub chunk. It is kept verbatim,
	// including unknown and non ASCII identifiers.
	ID [4]byte
	// Data is the raw content of the sub chunk with the optional word
	// alignment padding byte stripped.
	Data []byte
	// Truncated is true when the declared sub chunk size ran past the LIST
	// boundary; Data then holds only the bytes available up to the boundary
	// and the iteration stops after this entry.
	Truncated bool
	// MissingPad is true when the sub chunk has an odd size but no padding
	// byte was present before the LIST boundary (tolerated, not an error).
	MissingPad bool
}

// IsList reports whether the sub chunk is itself a nested LIST chunk.
// Nested lists are never traversed automatically; call SubIterator to
// descend into one.
func (sc *ListSubChunk) IsList() bool {
	return sc != nil && sc.ID == ListID
}

// ListType returns the 4-byte list type of a nested LIST sub chunk.
func (sc *ListSubChunk) ListType() ([4]byte, error) {
	var t [4]byte
	if sc == nil || len(sc.Data) < 4 {
		return t, errors.New("riff: nested LIST too small to hold a list type")
	}
	copy(t[:], sc.Data[:4])
	return t, nil
}

// SubIterator returns an iterator over the sub chunks of a nested LIST
// sub chunk. The sub chunk Data is expected to start with the 4-byte
// list type.
func (sc *ListSubChunk) SubIterator() *ListIterator {
	if sc == nil {
		return NewListIterator(nil, 0)
	}
	return NewListIterator(sc.Data, uint32(len(sc.Data)))
}

// ListIterator iterates over the sub chunks of a RIFF LIST chunk.
// The iteration is strictly bounded: it never reads past
// min(len(payload), declaredSize), so a LIST declaring more bytes than
// its parent holds cannot make the iterator overrun. Entries are
// yielded in their original order, duplicates and unknown identifiers
// included.
type ListIterator struct {
	payload []byte
	pos     int
	end     int
	cur     *ListSubChunk
	err     error
	pending error // deferred error, reported once iteration is exhausted
}

// NewListIterator creates an iterator over the payload of a LIST chunk.
// payload must start with the 4-byte list type (e.g. "INFO") and
// declaredSize is the size found in the LIST chunk header (which
// includes the 4 list type bytes). payload may be shorter than
// declaredSize when the parent boundary cuts the list short; in that
// case iteration covers the available bytes and Err reports
// ErrListTruncated once exhausted.
func NewListIterator(payload []byte, declaredSize uint32) *ListIterator {
	end := int(declaredSize)
	it := &ListIterator{payload: payload, end: end}
	if end > len(payload) {
		it.end = len(payload)
		it.pending = ErrListTruncated
	}
	if it.end >= 4 {
		it.pos = 4 // skip the list type
	} else {
		it.pos = it.end // too small even for a type: empty iteration
	}
	return it
}

// Type returns the 4-byte list type (e.g. InfoListID) of the iterated
// LIST chunk.
func (it *ListIterator) Type() ([4]byte, error) {
	var t [4]byte
	if it.end < 4 {
		return t, errors.New("riff: LIST payload too small to hold a list type")
	}
	copy(t[:], it.payload[:4])
	return t, nil
}

// Next advances to the next sub chunk. It returns false when the LIST
// boundary is reached or when a fatal condition is met; check Err after
// the loop.
func (it *ListIterator) Next() bool {
	if it.cur != nil && it.cur.Truncated {
		return false
	}
	if it.pos+8 > it.end {
		if it.pos < it.end {
			it.err = ErrListTrailingData
		} else if it.err == nil {
			it.err = it.pending
		}
		return false
	}
	var id [4]byte
	copy(id[:], it.payload[it.pos:it.pos+4])
	size := binary.LittleEndian.Uint32(it.payload[it.pos+4 : it.pos+8])
	start := it.pos + 8
	stop := start + int(size)
	sc := &ListSubChunk{ID: id}
	if stop > it.end {
		// The sub chunk claims more bytes than the LIST holds: clamp to
		// the boundary, flag the entry and stop after yielding it.
		sc.Truncated = true
		stop = it.end
		it.err = ErrListTruncated
	}
	sc.Data = it.payload[start:stop]
	next := stop
	if size%2 == 1 && !sc.Truncated {
		if stop < it.end {
			next = stop + 1 // skip the word alignment padding byte
		} else {
			sc.MissingPad = true
		}
	}
	it.pos = next
	it.cur = sc
	return true
}

// Chunk returns the sub chunk the iterator is currently positioned on.
func (it *ListIterator) Chunk() *ListSubChunk {
	return it.cur
}

// Err returns the first error encountered during iteration, if any.
// A truncated LIST still yields every readable entry before Err
// reports ErrListTruncated.
func (it *ListIterator) Err() error {
	return it.err
}

// InfoEntry is a single INFO metadata field. Unknown identifiers are
// preserved as-is so a read/rewrite cycle doesn't lose fields this
// package doesn't know about.
type InfoEntry struct {
	// CID is the 4-byte INFO identifier (e.g. 'I', 'N', 'A', 'M' for the
	// title). Non ASCII identifiers are kept verbatim.
	CID [4]byte
	// Value is the raw field value, usually a NUL terminated string.
	Value []byte
}

// String returns the value as a string with trailing NUL bytes trimmed.
func (e InfoEntry) String() string {
	return string(bytes.TrimRight(e.Value, "\x00"))
}

// InfoString builds an InfoEntry holding a NUL terminated string, the
// conventional encoding for INFO text fields.
func InfoString(cid [4]byte, value string) InfoEntry {
	return InfoEntry{CID: cid, Value: append([]byte(value), 0)}
}

// topChunk describes the byte span of one top level RIFF chunk.
type topChunk struct {
	id        [4]byte
	size      uint32
	start     int // offset of the chunk ID
	dataStart int
	dataEnd   int // end of the payload, clamped to the RIFF boundary
	end       int // including the padding byte, clamped to the boundary
}

// walkTopChunks splits the body of a RIFF container into its top level
// chunks. src must hold the whole container starting with the RIFF
// header. A declared RIFF size exceeding len(src) is clamped, and the
// returned boundary reflects the clamp.
func walkTopChunks(src []byte) (format [4]byte, chunks []topChunk, boundary int, err error) {
	if len(src) < 12 {
		return format, nil, 0, errors.New("riff: data too small to hold a RIFF header")
	}
	if !bytes.Equal(src[:4], RiffID[:]) {
		return format, nil, 0, fmt.Errorf("riff: %q - %s", src[:4], ErrFmtNotSupported)
	}
	declared := binary.LittleEndian.Uint32(src[4:8])
	copy(format[:], src[8:12])
	boundary = 8 + int(declared)
	if boundary > len(src) {
		boundary = len(src)
	}
	pos := 12
	for pos+8 <= boundary {
		var id [4]byte
		copy(id[:], src[pos:pos+4])
		size := binary.LittleEndian.Uint32(src[pos+4 : pos+8])
		dataStart := pos + 8
		dataEnd := dataStart + int(size)
		if dataEnd > boundary {
			dataEnd = boundary
		}
		chunkEnd := dataEnd
		if size%2 == 1 && chunkEnd < boundary {
			chunkEnd++
		}
		chunks = append(chunks, topChunk{
			id: id, size: size,
			start: pos, dataStart: dataStart, dataEnd: dataEnd, end: chunkEnd,
		})
		pos = chunkEnd
	}
	return format, chunks, boundary, nil
}

// ReadInfo returns the entries of the first top level LIST/INFO chunk
// of the RIFF container held in src, in their original order. Unknown,
// duplicate and non ASCII identifiers are preserved. It returns
// nil, nil when the container carries no INFO list.
func ReadInfo(src []byte) ([]InfoEntry, error) {
	_, chunks, _, err := walkTopChunks(src)
	if err != nil {
		return nil, err
	}
	for _, c := range chunks {
		if c.id != ListID {
			continue
		}
		payload := src[c.dataStart:c.dataEnd]
		it := NewListIterator(payload, c.size)
		t, err := it.Type()
		if err != nil || t != InfoListID {
			continue
		}
		var entries []InfoEntry
		for it.Next() {
			sc := it.Chunk()
			entries = append(entries, InfoEntry{
				CID:   sc.ID,
				Value: append([]byte(nil), sc.Data...),
			})
		}
		if err := it.Err(); err != nil {
			return entries, err
		}
		return entries, nil
	}
	return nil, nil
}

// SetInfo rewrites the first top level LIST/INFO chunk of the RIFF
// container held in src and returns the new container.
//
// Each edit replaces the value of the first entry carrying the same
// identifier (further duplicates are kept untouched) or is appended to
// the list when no entry carries it. Entries keep their original order
// and unknown fields are preserved. When the container has no LIST/INFO
// chunk, a new one is appended after the existing chunks.
//
// The RIFF and LIST sizes are recomputed and word alignment padding is
// regenerated for the rewritten list (a missing padding byte is
// normalized). The bytes of every other chunk, including the audio
// data chunk, are copied verbatim.
func SetInfo(src []byte, edits ...InfoEntry) ([]byte, error) {
	_, chunks, boundary, err := walkTopChunks(src)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.Grow(len(src) + 64)
	out.Write(src[:12]) // RIFF id + size (fixed below) + format

	rewritten := false
	pos := 12
	for _, c := range chunks {
		payload := src[c.dataStart:c.dataEnd]
		if !rewritten && c.id == ListID && len(payload) >= 4 &&
			bytes.Equal(payload[:4], InfoListID[:]) {
			writeChunk(&out, ListID, rewriteInfoPayload(payload, c.size, edits))
			rewritten = true
		} else {
			out.Write(src[c.start:c.end]) // verbatim, padding included
		}
		pos = c.end
	}
	// preserve any malformed trailing bytes inside the declared boundary
	if pos < boundary {
		out.Write(src[pos:boundary])
	}
	if !rewritten {
		entries := make([]InfoEntry, len(edits))
		for i, e := range edits {
			entries[i] = InfoEntry{CID: e.CID, Value: append([]byte(nil), e.Value...)}
		}
		writeChunk(&out, ListID, buildInfoPayload(entries))
	}

	outBytes := out.Bytes()
	binary.LittleEndian.PutUint32(outBytes[4:8], uint32(len(outBytes)-8))
	return outBytes, nil
}

// rewriteInfoPayload applies the edits to the entries of an existing
// INFO list payload and returns the rebuilt payload. The first entry
// matching an edit identifier is replaced, later duplicates are kept;
// unmatched edits are appended in order.
func rewriteInfoPayload(payload []byte, declaredSize uint32, edits []InfoEntry) []byte {
	it := NewListIterator(payload, declaredSize)
	var entries []InfoEntry
	for it.Next() {
		sc := it.Chunk()
		entries = append(entries, InfoEntry{
			CID:   sc.ID,
			Value: append([]byte(nil), sc.Data...),
		})
	}
	used := make([]bool, len(edits))
	for i := range entries {
		for j, ed := range edits {
			if !used[j] && ed.CID == entries[i].CID {
				entries[i].Value = append([]byte(nil), ed.Value...)
				used[j] = true
				break
			}
		}
	}
	for j, ed := range edits {
		if !used[j] {
			entries = append(entries, InfoEntry{
				CID:   ed.CID,
				Value: append([]byte(nil), ed.Value...),
			})
		}
	}
	return buildInfoPayload(entries)
}

// buildInfoPayload serializes INFO entries into a LIST payload starting
// with the INFO list type.
func buildInfoPayload(entries []InfoEntry) []byte {
	var buf bytes.Buffer
	buf.Write(InfoListID[:])
	for _, e := range entries {
		writeChunk(&buf, e.CID, e.Value)
	}
	return buf.Bytes()
}

// writeChunk writes a chunk header, its payload and the word alignment
// padding byte when the payload has an odd size.
func writeChunk(w *bytes.Buffer, id [4]byte, payload []byte) {
	w.Write(id[:])
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(payload)))
	w.Write(size[:])
	w.Write(payload)
	if len(payload)%2 == 1 {
		w.WriteByte(0)
	}
}
