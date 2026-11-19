package riff

import (
	"bytes"
	"encoding/binary"
	"os"
	"reflect"
	"testing"
)

// buildListPayload serializes entries into an INFO list payload for tests.
func buildListPayload(t *testing.T, listType [4]byte, entries []InfoEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(listType[:])
	for _, e := range entries {
		writeChunk(&buf, e.CID, e.Value)
	}
	return buf.Bytes()
}

func collectEntries(t *testing.T, it *ListIterator) []InfoEntry {
	t.Helper()
	var entries []InfoEntry
	for it.Next() {
		sc := it.Chunk()
		entries = append(entries, InfoEntry{CID: sc.ID, Value: sc.Data})
	}
	return entries
}

func TestListIteratorOrderAndUnknownFields(t *testing.T) {
	in := []InfoEntry{
		InfoString([4]byte{'I', 'N', 'A', 'M'}, "Old Recording"),
		InfoString([4]byte{'I', 'A', 'R', 'T'}, "Unknown Artist"),
		{CID: [4]byte{'X', 'Y', 'Z', 'W'}, Value: []byte("keep-me")},
	}
	payload := buildListPayload(t, InfoListID, in)

	it := NewListIterator(payload, uint32(len(payload)))
	typ, err := it.Type()
	if err != nil {
		t.Fatal(err)
	}
	if typ != InfoListID {
		t.Fatalf("list type didn't match %q, got %q", InfoListID, typ)
	}
	got := collectEntries(t, it)
	if it.Err() != nil {
		t.Fatalf("unexpected iterator error: %v", it.Err())
	}
	if len(got) != len(in) {
		t.Fatalf("expected %d entries, got %d", len(in), len(got))
	}
	for i, e := range in {
		if got[i].CID != e.CID {
			t.Fatalf("entry %d: CID didn't match %q, got %q", i, e.CID, got[i].CID)
		}
		if !bytes.Equal(got[i].Value, e.Value) {
			t.Fatalf("entry %d: value didn't match %q, got %q", i, e.Value, got[i].Value)
		}
	}
	if got[0].String() != "Old Recording" {
		t.Fatalf("String() didn't trim the NUL terminator, got %q", got[0].String())
	}
}

func TestListIteratorNestedList(t *testing.T) {
	inner := buildListPayload(t, [4]byte{'a', 'd', 't', 'l'}, []InfoEntry{
		InfoString([4]byte{'I', 'N', 'A', 'M'}, "inner"),
	})
	outer := buildListPayload(t, InfoListID, []InfoEntry{
		InfoString([4]byte{'I', 'A', 'R', 'T'}, "outer artist"),
		{CID: ListID, Value: inner},
	})

	it := NewListIterator(outer, uint32(len(outer)))
	var nested *ListSubChunk
	count := 0
	for it.Next() {
		sc := it.Chunk()
		count++
		if sc.IsList() {
			nested = sc
		}
	}
	if it.Err() != nil {
		t.Fatalf("unexpected iterator error: %v", it.Err())
	}
	if count != 2 {
		t.Fatalf("expected 2 sub chunks, got %d", count)
	}
	if nested == nil {
		t.Fatal("nested LIST sub chunk not reported as a list")
	}
	lt, err := nested.ListType()
	if err != nil {
		t.Fatal(err)
	}
	if lt != [4]byte{'a', 'd', 't', 'l'} {
		t.Fatalf("nested list type didn't match, got %q", lt)
	}
	sub := collectEntries(t, nested.SubIterator())
	if len(sub) != 1 || sub[0].String() != "inner" {
		t.Fatalf("nested entries didn't match, got %+v", sub)
	}
}

func TestListIteratorZeroLength(t *testing.T) {
	payload := buildListPayload(t, InfoListID, []InfoEntry{
		{CID: [4]byte{'I', 'C', 'M', 'T'}, Value: nil},
		InfoString([4]byte{'I', 'N', 'A', 'M'}, "after empty"),
	})
	it := NewListIterator(payload, uint32(len(payload)))
	got := collectEntries(t, it)
	if it.Err() != nil {
		t.Fatalf("unexpected iterator error: %v", it.Err())
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if len(got[0].Value) != 0 {
		t.Fatalf("expected a zero length value, got %q", got[0].Value)
	}
	if got[1].String() != "after empty" {
		t.Fatalf("iteration didn't resume after a zero length chunk, got %q", got[1].String())
	}
}

func TestListIteratorMissingPadding(t *testing.T) {
	// odd sized entry ending exactly on the LIST boundary: no room for the
	// padding byte. Tolerated and flagged, not an error.
	var payload []byte
	payload = append(payload, InfoListID[:]...)
	payload = append(payload, 'I', 'N', 'A', 'M')
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], 3)
	payload = append(payload, sz[:]...)
	payload = append(payload, 'a', 'b', 'c')

	it := NewListIterator(payload, uint32(len(payload)))
	if !it.Next() {
		t.Fatal("expected one entry")
	}
	sc := it.Chunk()
	if !sc.MissingPad {
		t.Fatal("expected the missing padding byte to be flagged")
	}
	if string(sc.Data) != "abc" {
		t.Fatalf("data didn't match, got %q", sc.Data)
	}
	if it.Next() {
		t.Fatal("expected iteration to stop at the boundary")
	}
	if it.Err() != nil {
		t.Fatalf("missing padding should be tolerated, got %v", it.Err())
	}
}

func TestListIteratorDeclaredSizeBeyondParent(t *testing.T) {
	payload := buildListPayload(t, InfoListID, []InfoEntry{
		InfoString([4]byte{'I', 'N', 'A', 'M'}, "short"),
	})
	// the LIST header claims far more than the parent actually holds
	it := NewListIterator(payload, uint32(len(payload))+512)
	got := collectEntries(t, it)
	if len(got) != 1 || got[0].String() != "short" {
		t.Fatalf("expected the readable entry to be yielded, got %+v", got)
	}
	if it.Err() != ErrListTruncated {
		t.Fatalf("expected ErrListTruncated, got %v", it.Err())
	}
}

func TestListIteratorTruncatedSubChunk(t *testing.T) {
	// a sub chunk whose own declared size runs past the LIST boundary
	var payload []byte
	payload = append(payload, InfoListID[:]...)
	payload = append(payload, 'I', 'N', 'A', 'M')
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], 100)
	payload = append(payload, sz[:]...)
	payload = append(payload, []byte("partial")...)

	it := NewListIterator(payload, uint32(len(payload)))
	if !it.Next() {
		t.Fatal("expected the truncated entry to be yielded")
	}
	sc := it.Chunk()
	if !sc.Truncated {
		t.Fatal("expected the entry to be flagged as truncated")
	}
	if string(sc.Data) != "partial" {
		t.Fatalf("expected clamped data, got %q", sc.Data)
	}
	if it.Next() {
		t.Fatal("iteration must stop after a truncated sub chunk")
	}
	if it.Err() != ErrListTruncated {
		t.Fatalf("expected ErrListTruncated, got %v", it.Err())
	}
}

func TestListIteratorDuplicateAndNonASCII(t *testing.T) {
	nonASCII := [4]byte{0xff, 0x00, 'Z', 'Z'}
	payload := buildListPayload(t, InfoListID, []InfoEntry{
		InfoString([4]byte{'I', 'N', 'A', 'M'}, "first"),
		InfoString([4]byte{'I', 'N', 'A', 'M'}, "second"),
		{CID: nonASCII, Value: []byte{0x01, 0x02}},
	})
	it := NewListIterator(payload, uint32(len(payload)))
	got := collectEntries(t, it)
	if it.Err() != nil {
		t.Fatalf("unexpected iterator error: %v", it.Err())
	}
	if len(got) != 3 {
		t.Fatalf("duplicates must be preserved in order, got %d entries", len(got))
	}
	if got[0].String() != "first" || got[1].String() != "second" {
		t.Fatalf("duplicate order not preserved: %q, %q", got[0].String(), got[1].String())
	}
	if got[2].CID != nonASCII {
		t.Fatalf("non ASCII identifier not preserved, got %q", got[2].CID)
	}
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	src, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestReadInfoFixture(t *testing.T) {
	src := loadFixture(t, "fixtures/infoList.wav")
	entries, err := ReadInfo(src)
	if err != nil {
		t.Fatal(err)
	}
	want := []InfoEntry{
		InfoString([4]byte{'I', 'N', 'A', 'M'}, "Old Recording"),
		InfoString([4]byte{'I', 'A', 'R', 'T'}, "Unknown Artist"),
		{CID: [4]byte{'X', 'Y', 'Z', 'W'}, Value: []byte("keep-me")},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("INFO entries didn't match\nwant: %+v\ngot:  %+v", want, entries)
	}
}

// chunkPayload locates a top level chunk by ID and returns its payload bytes.
func chunkPayload(t *testing.T, src []byte, id [4]byte) []byte {
	t.Helper()
	_, chunks, _, err := walkTopChunks(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if c.id == id {
			return src[c.dataStart:c.dataEnd]
		}
	}
	return nil
}

func TestSetInfoRewrite(t *testing.T) {
	src := loadFixture(t, "fixtures/infoList.wav")

	out, err := SetInfo(src,
		InfoString([4]byte{'I', 'A', 'R', 'T'}, "Jane Doe"),
		InfoString([4]byte{'I', 'C', 'R', 'D'}, "1978"),
	)
	if err != nil {
		t.Fatal(err)
	}

	// INFO content: replaced in place, appended at the end, unknown kept
	entries, err := ReadInfo(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []InfoEntry{
		InfoString([4]byte{'I', 'N', 'A', 'M'}, "Old Recording"),
		InfoString([4]byte{'I', 'A', 'R', 'T'}, "Jane Doe"),
		{CID: [4]byte{'X', 'Y', 'Z', 'W'}, Value: []byte("keep-me")},
		InfoString([4]byte{'I', 'C', 'R', 'D'}, "1978"),
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("rewritten INFO didn't match\nwant: %+v\ngot:  %+v", want, entries)
	}

	// sizes: RIFF size covers the whole file minus its own header
	if got := binary.LittleEndian.Uint32(out[4:8]); got != uint32(len(out)-8) {
		t.Fatalf("RIFF size didn't match %d, got %d", len(out)-8, got)
	}
	// every chunk boundary is consistent and word aligned
	_, chunks, boundary, err := walkTopChunks(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 || chunks[len(chunks)-1].end != boundary {
		t.Fatalf("chunk walk didn't end exactly on the RIFF boundary (%d)", boundary)
	}

	// untouched chunks are byte identical
	for _, id := range [][4]byte{junkID, FmtID, DataFormatID} {
		before := chunkPayload(t, src, id)
		after := chunkPayload(t, out, id)
		if !bytes.Equal(before, after) {
			t.Fatalf("chunk %q changed: %x -> %x", id, before, after)
		}
	}

	// the rewritten container still parses with the existing parser
	p := New(bytes.NewReader(out))
	if err := p.ParseHeaders(); err != nil {
		t.Fatal(err)
	}
	if p.Format != WavFormatID {
		t.Fatalf("format didn't match WAVE, got %q", p.Format)
	}
	if err := p.Parse(); err != nil {
		t.Fatal(err)
	}
	if p.NumChannels != 1 || p.SampleRate != 8000 || p.BitsPerSample != 8 {
		t.Fatalf("wav header didn't survive the rewrite: %d ch @ %d / %d bits",
			p.NumChannels, p.SampleRate, p.BitsPerSample)
	}
}

func TestSetInfoDuplicateIDs(t *testing.T) {
	payload := buildListPayload(t, InfoListID, []InfoEntry{
		InfoString([4]byte{'I', 'N', 'A', 'M'}, "first"),
		InfoString([4]byte{'I', 'N', 'A', 'M'}, "second"),
	})
	var src []byte
	src = append(src, RiffID[:]...)
	src = append(src, 0, 0, 0, 0)
	src = append(src, WavFormatID[:]...)
	var listBuf bytes.Buffer
	writeChunk(&listBuf, ListID, payload)
	src = append(src, listBuf.Bytes()...)
	binary.LittleEndian.PutUint32(src[4:8], uint32(len(src)-8))

	out, err := SetInfo(src, InfoString([4]byte{'I', 'N', 'A', 'M'}, "replaced"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ReadInfo(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected the duplicate to be kept, got %+v", entries)
	}
	if entries[0].String() != "replaced" || entries[1].String() != "second" {
		t.Fatalf("only the first duplicate should be replaced, got %q / %q",
			entries[0].String(), entries[1].String())
	}
}

func TestSetInfoAppendsListWhenMissing(t *testing.T) {
	src := loadFixture(t, "fixtures/infoList.wav")
	// strip the LIST chunk to build a WAV without INFO
	_, chunks, _, err := walkTopChunks(src)
	if err != nil {
		t.Fatal(err)
	}
	var bare bytes.Buffer
	bare.Write(src[:12])
	for _, c := range chunks {
		if c.id == ListID {
			continue
		}
		bare.Write(src[c.start:c.end])
	}
	bareBytes := bare.Bytes()
	binary.LittleEndian.PutUint32(bareBytes[4:8], uint32(len(bareBytes)-8))

	out, err := SetInfo(bareBytes, InfoString([4]byte{'I', 'N', 'A', 'M'}, "added"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ReadInfo(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].String() != "added" {
		t.Fatalf("expected the new INFO list, got %+v", entries)
	}
	if got := binary.LittleEndian.Uint32(out[4:8]); got != uint32(len(out)-8) {
		t.Fatalf("RIFF size didn't match %d, got %d", len(out)-8, got)
	}
	if !bytes.Equal(chunkPayload(t, src, DataFormatID), chunkPayload(t, out, DataFormatID)) {
		t.Fatal("data chunk changed while appending a LIST")
	}
}

func TestSetInfoByteRoundTrip(t *testing.T) {
	src := loadFixture(t, "fixtures/infoList.wav")
	// replacing a value with one of the same length must leave every byte
	// outside the INFO value untouched
	out, err := SetInfo(src, InfoString([4]byte{'I', 'N', 'A', 'M'}, "New Recording"))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(src) {
		t.Fatalf("same length replacement changed the file size: %d -> %d", len(src), len(out))
	}
	// the INAM value spans 0x46..0x54 in the fixture (see hexdump)
	if !bytes.Equal(out[:0x46], src[:0x46]) || !bytes.Equal(out[0x54:], src[0x54:]) {
		t.Fatal("bytes outside the edited INFO value changed")
	}
}
