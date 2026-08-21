# Engine Index Sharing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let one machine publish a prebuilt clangd engine index to GitHub releases and let another machine find, fetch, and install it against its own engine path.

**Architecture:** A published index is path-free. Publishing rewrites every absolute root in the index's string table to a fixed placeholder; installing rewrites the placeholders back to local paths. Matching is done on a path-independent engine identity derived from `Build.version` plus a structural hash of engine-relative paths and path-normalised compile arguments, refined by distribution-specific discriminators.

**Tech Stack:** Go 1.25, standard library only for new work (`compress/zlib`, `compress/gzip`, `crypto/sha256`, `encoding/binary`, `encoding/json`, `net/http`), plus shelling out to `gh` for upload.

**Spec:** `docs/superpowers/specs/2026-08-21-engine-index-sharing-design.md`

## Global Constraints

- Module is `github.com/softdaddy-o/soft-ue-index`, Go `1.25.0`. **Add no new third-party dependency.** Current requires: go-winio, fsnotify, mcp-sdk, `golang.org/x/sys`.
- Every task is TDD: write the failing test, run it and see it fail, implement, run it and see it pass, commit.
- After every task: `gofmt -l ./...` (must print nothing), `go build ./...`, `go vet ./...`, `go test ./...` must all be clean.
- Placeholder names are exactly `ENGINE_PATH`, `SDK_PATH`, `MSVC_PATH`, `CLANG_PATH`.
- Index format facts, verified against a real 539 MB index and clangd source: RIFF form type is `CdIx`; chunks observed are `meta`(4 bytes, index format version, value 20 for clangd 20.1.8), `stri`, `symb`, `refs`, `rela`, `srcs`. All strings live only in `stri`; all other chunks reference them by varint index.
- `stri` layout: a fixed **32-bit little-endian** header. `0` means the remainder is the raw NUL-separated table; any other value means the remainder is zlib-compressed and that value is the uncompressed size. Writers emit compressed only when their clangd build had zlib. **Re-emitted tables are always written raw (header 0).**
- Rewriting is a re-serialisation, never an in-place byte patch. `meta`, `symb`, `refs`, `rela`, `srcs` must pass through byte-for-byte.
- A compile-argument token that still holds an absolute path after substitution is a hard error, never a pass-through.
- Never write a local path, username, or raw marker-file content into a manifest.

---

## File Structure

**New package `internal/idxrewrite`** — the clangd index file format. Knows nothing about engines or GitHub.

- `riff.go` — RIFF container parse and re-emit.
- `stringtable.go` — `stri` decode (raw or zlib) and encode (raw).
- `placeholder.go` — placeholder names, the `Roots` struct, URI encode/decode with offset mapping.
- `rewrite.go` — `Normalize`, `Denormalize`, `AbsolutePathResidue`.

**New package `internal/engineid`** — path-independent engine identity. Depends on `compdb` and `idxrewrite`.

- `buildversion.go` — parse `Engine/Build/Build.version`.
- `argpaths.go` — path-bearing flag table, substitution, residual absolute-path detection.
- `structural.go` — the structural hash.
- `discriminator.go` — pluggable distribution adapters.
- `identity.go` — identity assembly and the match policy.

**New package `internal/indexshare`** — manifest, packaging, transport, orchestration.

- `manifest.go` — schema, validation, compatibility gates.
- `pack.go` — gzip and SHA-256 packaging.
- `release.go` — GitHub release listing and download over `net/http`, upload via `gh`.
- `share.go` — `List`, `Pull`, `Push`.

**Modified**

- `internal/cli/cli.go` — parse `engine-index <action> <project>` and its flags.
- `internal/app/app.go` — dispatch the new command, add the remote check to `index-engine`.
- `README.md` — document the workflow.

---

## Task 1: RIFF container round-trip

**Files:**
- Create: `internal/idxrewrite/riff.go`
- Test: `internal/idxrewrite/riff_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Chunk struct { ID string; Data []byte }`, `type Container struct { FormType string; Chunks []Chunk }`, `func Parse(b []byte) (*Container, error)`, `func (c *Container) Marshal() []byte`, `func (c *Container) Find(id string) (*Chunk, bool)`.

- [ ] **Step 1: Write the failing test**

```go
package idxrewrite

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildRIFF assembles a RIFF file the way clangd's serializer does:
// "RIFF", uint32 payload size, 4-byte form type, then chunks, each
// padded to an even length.
func buildRIFF(t *testing.T, formType string, chunks []Chunk) []byte {
	t.Helper()
	var payload bytes.Buffer
	payload.WriteString(formType)
	for _, c := range chunks {
		payload.WriteString(c.ID)
		var size [4]byte
		binary.LittleEndian.PutUint32(size[:], uint32(len(c.Data)))
		payload.Write(size[:])
		payload.Write(c.Data)
		if len(c.Data)%2 == 1 {
			payload.WriteByte(0)
		}
	}
	var out bytes.Buffer
	out.WriteString("RIFF")
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(payload.Len()))
	out.Write(size[:])
	out.Write(payload.Bytes())
	return out.Bytes()
}

func TestParseReadsFormTypeAndChunks(t *testing.T) {
	raw := buildRIFF(t, "CdIx", []Chunk{
		{ID: "meta", Data: []byte{20, 0, 0, 0}},
		{ID: "stri", Data: []byte("abc")},
	})
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.FormType != "CdIx" {
		t.Fatalf("form type = %q, want CdIx", got.FormType)
	}
	if len(got.Chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(got.Chunks))
	}
	stri, ok := got.Find("stri")
	if !ok || string(stri.Data) != "abc" {
		t.Fatalf("stri chunk = %q ok=%v, want abc true", stri, ok)
	}
}

// An odd-sized chunk is followed by a pad byte that is not part of its
// data. Losing that distinction corrupts every following chunk offset.
func TestMarshalRoundTripsOddSizedChunksByteForByte(t *testing.T) {
	raw := buildRIFF(t, "CdIx", []Chunk{
		{ID: "meta", Data: []byte{20, 0, 0, 0}},
		{ID: "stri", Data: []byte("odd")},
		{ID: "refs", Data: []byte("payload")},
	})
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := parsed.Marshal(); !bytes.Equal(got, raw) {
		t.Fatalf("round trip changed %d bytes into %d", len(raw), len(got))
	}
}

func TestParseRejectsNonRIFF(t *testing.T) {
	if _, err := Parse([]byte("NOPE\x00\x00\x00\x00CdIx")); err == nil {
		t.Fatal("Parse accepted a non-RIFF header")
	}
}

func TestParseRejectsChunkSizeBeyondEndOfFile(t *testing.T) {
	raw := buildRIFF(t, "CdIx", []Chunk{{ID: "stri", Data: []byte("abc")}})
	// Inflate the stri chunk's declared size past the end of the buffer.
	binary.LittleEndian.PutUint32(raw[16:20], 1<<20)
	if _, err := Parse(raw); err == nil {
		t.Fatal("Parse accepted a chunk size past end of file")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/idxrewrite && go test ./... -run TestParse -v`
Expected: FAIL to compile, `undefined: Chunk`, `undefined: Parse`.

- [ ] **Step 3: Write minimal implementation**

```go
// Package idxrewrite reads and rewrites clangd's monolithic index format.
//
// clangd cannot relocate a monolithic index itself: Index.External's File
// branch passes only the file location to loadIndex, and MountPoint reaches
// nothing but a log call. clangd-indexer has no relative-path option, and
// getCanonicalPath deliberately resolves symlinks, so alias-path tricks do
// not help either. Rewriting the index is therefore the only way to move a
// prebuilt index between machines, and it is tractable because every path
// lives in one deduplicated string table that other chunks address by index.
package idxrewrite

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// FormType is the RIFF form type clangd writes for a serialized index.
const FormType = "CdIx"

// ErrNotAnIndex reports input that is not a clangd RIFF index.
var ErrNotAnIndex = errors.New("not a clangd index file")

// Chunk is one RIFF chunk. Data excludes RIFF's even-length pad byte.
type Chunk struct {
	ID   string
	Data []byte
}

// Container is a parsed RIFF file.
type Container struct {
	FormType string
	Chunks   []Chunk
}

// Find returns the first chunk with the given id.
func (c *Container) Find(id string) (*Chunk, bool) {
	for i := range c.Chunks {
		if c.Chunks[i].ID == id {
			return &c.Chunks[i], true
		}
	}
	return nil, false
}

// Parse reads a RIFF container. Chunk data is aliased into b, not copied;
// callers that mutate a chunk must replace Data rather than write through it.
func Parse(b []byte) (*Container, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" {
		return nil, fmt.Errorf("%w: missing RIFF header", ErrNotAnIndex)
	}
	declared := binary.LittleEndian.Uint32(b[4:8])
	if uint64(declared)+8 > uint64(len(b)) {
		return nil, fmt.Errorf("%w: declared size %d exceeds %d bytes", ErrNotAnIndex, declared, len(b))
	}
	end := 8 + int(declared)
	out := &Container{FormType: string(b[8:12])}
	for pos := 12; pos < end; {
		if pos+8 > end {
			return nil, fmt.Errorf("%w: truncated chunk header at %d", ErrNotAnIndex, pos)
		}
		id := string(b[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		pos += 8
		if size < 0 || pos+size > end {
			return nil, fmt.Errorf("%w: chunk %q size %d overruns the container", ErrNotAnIndex, id, size)
		}
		out.Chunks = append(out.Chunks, Chunk{ID: id, Data: b[pos : pos+size]})
		pos += size
		if size%2 == 1 {
			pos++
		}
	}
	return out, nil
}

// Marshal re-emits the container, recomputing every chunk size, the
// top-level size, and RIFF's even-length padding. Rewriting a string table
// changes its length, so emitting must always go through this rather than
// patching bytes in place.
func (c *Container) Marshal() []byte {
	payload := 4
	for _, ch := range c.Chunks {
		payload += 8 + len(ch.Data)
		if len(ch.Data)%2 == 1 {
			payload++
		}
	}
	out := make([]byte, 0, 8+payload)
	out = append(out, "RIFF"...)
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(payload))
	out = append(out, size[:]...)
	out = append(out, c.FormType...)
	for _, ch := range c.Chunks {
		out = append(out, ch.ID...)
		binary.LittleEndian.PutUint32(size[:], uint32(len(ch.Data)))
		out = append(out, size[:]...)
		out = append(out, ch.Data...)
		if len(ch.Data)%2 == 1 {
			out = append(out, 0)
		}
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/idxrewrite/... -v`
Expected: PASS, 4 tests.

- [ ] **Step 5: Verify against a real index if one exists on this machine**

A synthetic fixture cannot prove agreement with a file clangd actually wrote. Add this test, which skips itself when no real index is available, and keep it — it is the only check that ties the parser to the real format.

```go
func TestParseAgreesWithARealClangdIndex(t *testing.T) {
	path := os.Getenv("SOFT_UE_INDEX_TEST_IDX")
	if path == "" {
		t.Skip("set SOFT_UE_INDEX_TEST_IDX to a real clangd index to run this")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.FormType != FormType {
		t.Fatalf("form type = %q, want %q", parsed.FormType, FormType)
	}
	if _, ok := parsed.Find("stri"); !ok {
		t.Fatal("real index has no stri chunk")
	}
	if got := parsed.Marshal(); !bytes.Equal(got, raw) {
		t.Fatalf("re-emitting a real index changed it (%d bytes in, %d out)", len(raw), len(got))
	}
}
```

The chunk id is spelled literally so this test compiles in Task 1, before Task 2 introduces `StringTableChunkID`. Add `"os"` and `"bytes"` to the test file's imports.

Run: `SOFT_UE_INDEX_TEST_IDX=<path to a real engine.idx> go test ./internal/idxrewrite/... -run RealClangdIndex -v`
Expected: PASS, or SKIP when the variable is unset.

- [ ] **Step 6: Run the full gate and commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/idxrewrite/riff.go internal/idxrewrite/riff_test.go
git commit -m "feat(idxrewrite): parse and re-emit clangd index RIFF containers"
```

---

## Task 2: String table codec

**Files:**
- Create: `internal/idxrewrite/stringtable.go`
- Test: `internal/idxrewrite/stringtable_test.go`

**Interfaces:**
- Consumes: Task 1's `Chunk`.
- Produces: `func DecodeStringTable(chunk []byte) ([][]byte, error)`, `func EncodeStringTable(entries [][]byte) []byte`.

The header is 32-bit little-endian. `0` means the rest is the raw table; anything else means the rest is zlib-compressed to that uncompressed size. Both forms occur in the wild because clangd emits compressed only when its build had zlib. Encoding always writes raw, because a compressed table is unreadable to a clangd built without zlib and the artifact is gzipped for transport anyway.

- [ ] **Step 1: Write the failing test**

```go
package idxrewrite

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"testing"
)

func rawTable(entries ...string) []byte {
	var body bytes.Buffer
	for _, e := range entries {
		body.WriteString(e)
		body.WriteByte(0)
	}
	out := make([]byte, 4, 4+body.Len())
	binary.LittleEndian.PutUint32(out[0:4], 0)
	return append(out, body.Bytes()...)
}

func zlibTable(t *testing.T, entries ...string) []byte {
	t.Helper()
	var body bytes.Buffer
	for _, e := range entries {
		body.WriteString(e)
		body.WriteByte(0)
	}
	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	if _, err := w.Write(body.Bytes()); err != nil {
		t.Fatalf("compress: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	out := make([]byte, 4, 4+compressed.Len())
	binary.LittleEndian.PutUint32(out[0:4], uint32(body.Len()))
	return append(out, compressed.Bytes()...)
}

func TestDecodeStringTableReadsRawForm(t *testing.T) {
	got, err := DecodeStringTable(rawTable("", "alpha", "beta"))
	if err != nil {
		t.Fatalf("DecodeStringTable: %v", err)
	}
	want := []string{"", "alpha", "beta"}
	if len(got) != len(want) {
		t.Fatalf("entries = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A zlib-compressed table is what any clangd built with zlib produces.
// Assuming the raw form would corrupt or silently skip those indexes.
func TestDecodeStringTableReadsZlibForm(t *testing.T) {
	got, err := DecodeStringTable(zlibTable(t, "", "alpha", "beta"))
	if err != nil {
		t.Fatalf("DecodeStringTable: %v", err)
	}
	if len(got) != 3 || string(got[1]) != "alpha" {
		t.Fatalf("entries = %q", got)
	}
}

func TestEncodeStringTableAlwaysEmitsRaw(t *testing.T) {
	out := EncodeStringTable([][]byte{[]byte(""), []byte("alpha")})
	if got := binary.LittleEndian.Uint32(out[0:4]); got != 0 {
		t.Fatalf("header = %d, want 0 (raw)", got)
	}
	if !bytes.Equal(out, rawTable("", "alpha")) {
		t.Fatalf("encoded = %q, want %q", out, rawTable("", "alpha"))
	}
}

func TestEncodeDecodeRoundTripsBothInputForms(t *testing.T) {
	for name, table := range map[string][]byte{
		"raw":  rawTable("", "alpha", "beta"),
		"zlib": zlibTable(t, "", "alpha", "beta"),
	} {
		entries, err := DecodeStringTable(table)
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		again, err := DecodeStringTable(EncodeStringTable(entries))
		if err != nil {
			t.Fatalf("%s: re-decode: %v", name, err)
		}
		if len(again) != len(entries) {
			t.Fatalf("%s: %d entries after round trip, want %d", name, len(again), len(entries))
		}
		for i := range entries {
			if !bytes.Equal(again[i], entries[i]) {
				t.Fatalf("%s: entry %d = %q, want %q", name, i, again[i], entries[i])
			}
		}
	}
}

func TestDecodeStringTableRejectsShortChunk(t *testing.T) {
	if _, err := DecodeStringTable([]byte{0, 0}); err == nil {
		t.Fatal("DecodeStringTable accepted a chunk shorter than its header")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/idxrewrite/... -run StringTable -v`
Expected: FAIL to compile, `undefined: DecodeStringTable`.

- [ ] **Step 3: Write minimal implementation**

```go
package idxrewrite

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
)

// StringTableChunkID is the RIFF chunk holding every string in the index.
const StringTableChunkID = "stri"

// Bounds applied to the string table before allocating for it. The measured
// real index has a 59 MB table, so 1 GB is generous headroom while still
// refusing an artifact that asks for the address space.
const (
	MaxStringTableBytes  = 1 << 30
	MaxCompressionRatio  = 1032
)

// DecodeStringTable splits the string table chunk into its NUL-terminated
// entries. The chunk begins with a 32-bit little-endian uncompressed size:
// zero means the remainder is the raw table, and any other value means the
// remainder is zlib-compressed to exactly that size. Which form a writer
// emits depends on whether its clangd build had zlib, so both occur.
func DecodeStringTable(chunk []byte) ([][]byte, error) {
	if len(chunk) < 4 {
		return nil, fmt.Errorf("%w: string table chunk is %d bytes, need at least 4", ErrNotAnIndex, len(chunk))
	}
	uncompressedSize := binary.LittleEndian.Uint32(chunk[0:4])
	body := chunk[4:]
	if uncompressedSize != 0 {
		// That header is four attacker-controlled bytes that select an
		// allocation size, so it is checked before it is trusted. clangd's own
		// reader applies the same kind of plausibility bound. Without this a
		// few-kilobyte download can ask for gigabytes.
		if uint64(uncompressedSize) > MaxStringTableBytes {
			return nil, fmt.Errorf("%w: string table declares %d bytes, over the %d byte ceiling", ErrNotAnIndex, uncompressedSize, uint64(MaxStringTableBytes))
		}
		if uint64(uncompressedSize) > uint64(len(body))*MaxCompressionRatio {
			return nil, fmt.Errorf("%w: string table declares %d bytes from %d compressed, an implausible ratio", ErrNotAnIndex, uncompressedSize, len(body))
		}
		r, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("open compressed string table: %w", err)
		}
		defer r.Close()
		out := make([]byte, uncompressedSize)
		if _, err := io.ReadFull(r, out); err != nil {
			return nil, fmt.Errorf("decompress string table: %w", err)
		}
		// The stream must end exactly here. A stream that keeps going is not
		// the table it claimed to be.
		var extra [1]byte
		if n, err := r.Read(extra[:]); n != 0 || err != io.EOF {
			return nil, fmt.Errorf("%w: string table expands past its declared %d bytes", ErrNotAnIndex, uncompressedSize)
		}
		body = out
	}
	if len(body) == 0 {
		return nil, nil
	}
	if body[len(body)-1] != 0 {
		return nil, fmt.Errorf("%w: string table does not end with a terminator", ErrNotAnIndex)
	}
	return bytes.Split(body[:len(body)-1], []byte{0}), nil
}

// EncodeStringTable serialises entries as a raw table. It never compresses:
// a compressed table cannot be read by a clangd built without zlib, and the
// published artifact is gzipped for transport regardless, so emitting raw
// costs nothing and widens compatibility.
func EncodeStringTable(entries [][]byte) []byte {
	size := 4
	for _, e := range entries {
		size += len(e) + 1
	}
	out := make([]byte, 4, size)
	binary.LittleEndian.PutUint32(out[0:4], 0)
	for _, e := range entries {
		out = append(out, e...)
		out = append(out, 0)
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/idxrewrite/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/idxrewrite/stringtable.go internal/idxrewrite/stringtable_test.go
git commit -m "feat(idxrewrite): decode raw and zlib string tables, always re-emit raw"
```

---

## Task 3: URI placeholders and offset-preserving rewriting

**Files:**
- Create: `internal/idxrewrite/placeholder.go`
- Test: `internal/idxrewrite/placeholder_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: constants `EnginePlaceholder`, `SDKPlaceholder`, `MSVCPlaceholder`, `ClangPlaceholder`; `type Roots struct { Engine, SDK, MSVC, Clang string }`; `func (r Roots) Each() []RootBinding`; `type RootBinding struct { Placeholder, Path string }`; `func EncodeURI(path string) string`; `func DecodeURIPrefixLen(uri string, decodedLen int) int`; `func DecodeURI(uri string) string`.

clangd escapes every byte outside `A-Za-z0-9-_.~/:` when it builds a file URI. That was confirmed against a real index, where `C:/Program Files (x86)/Windows Kits` appears as `C:/Program%20Files%20%28x86%29/Windows%20Kits` — space, `(`, and `)` escaped, while `:` and `/` are not.

Rewriting keeps the encoded tail of each URI verbatim and replaces only the prefix, so no re-encoding of the tail can drift. That requires mapping a decoded prefix length back to an encoded offset, which is what `DecodeURIPrefixLen` does.

- [ ] **Step 1: Write the failing test**

```go
package idxrewrite

import "testing"

func TestEncodeURIMatchesClangdEscaping(t *testing.T) {
	got := EncodeURI("C:/Program Files (x86)/Windows Kits")
	want := "file:///C:/Program%20Files%20%28x86%29/Windows%20Kits"
	if got != want {
		t.Fatalf("EncodeURI = %q, want %q", got, want)
	}
}

func TestEncodeURILeavesUnreservedAndPathPunctuationAlone(t *testing.T) {
	got := EncodeURI("D:/Elpis_UE5.8/Engine/Source/a-b~c.cpp")
	want := "file:///D:/Elpis_UE5.8/Engine/Source/a-b~c.cpp"
	if got != want {
		t.Fatalf("EncodeURI = %q, want %q", got, want)
	}
}

func TestDecodeURIReversesEscaping(t *testing.T) {
	got := DecodeURI("file:///C:/Program%20Files%20%28x86%29/x.h")
	want := "C:/Program Files (x86)/x.h"
	if got != want {
		t.Fatalf("DecodeURI = %q, want %q", got, want)
	}
}

// The rewriter replaces an encoded prefix without touching the tail, so it
// must know how many encoded bytes a decoded prefix spans. Getting this
// wrong silently truncates or duplicates path segments.
func TestDecodeURIPrefixLenMapsDecodedLengthToEncodedOffset(t *testing.T) {
	uri := "file:///C:/Program%20Files%20%28x86%29/Windows%20Kits/x.h"
	root := "C:/Program Files (x86)/Windows Kits"
	n := DecodeURIPrefixLen(uri, len(root))
	if got, want := uri[:n], "file:///C:/Program%20Files%20%28x86%29/Windows%20Kits"; got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}
	if got, want := uri[n:], "/x.h"; got != want {
		t.Fatalf("tail = %q, want %q", got, want)
	}
}

func TestDecodeURIPrefixLenHandlesBoundariesAroundAnEscape(t *testing.T) {
	uri := "file:///C:/a%20b/x.h"
	// Immediately before the escape.
	if got, want := DecodeURIPrefixLen(uri, len("C:/a")), len("file:///C:/a"); got != want {
		t.Fatalf("offset before escape = %d, want %d", got, want)
	}
	// Immediately after it: one decoded byte spans three encoded bytes.
	if got, want := DecodeURIPrefixLen(uri, len("C:/a ")), len("file:///C:/a%20"); got != want {
		t.Fatalf("offset after escape = %d, want %d", got, want)
	}
	// Past the end has no offset at all.
	if got := DecodeURIPrefixLen(uri, 999); got != -1 {
		t.Fatalf("out-of-range decoded length returned %d, want -1", got)
	}
}

func TestDecodeURIPrefixLenRejectsANonFileURI(t *testing.T) {
	if got := DecodeURIPrefixLen("http://example/x", 3); got != -1 {
		t.Fatalf("non-file URI returned %d, want -1", got)
	}
}

func TestRootsEachSkipsUnsetRoots(t *testing.T) {
	r := Roots{Engine: "D:/Engine", SDK: ""}
	bindings := r.Each()
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(bindings))
	}
	if bindings[0].Placeholder != EnginePlaceholder || bindings[0].Path != "D:/Engine" {
		t.Fatalf("binding = %+v", bindings[0])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/idxrewrite/... -run 'URI|Roots' -v`
Expected: FAIL to compile, `undefined: EncodeURI`.

- [ ] **Step 3: Write minimal implementation**

```go
package idxrewrite

import "strings"

// Placeholder names substituted for absolute roots in a published index.
const (
	EnginePlaceholder = "ENGINE_PATH"
	SDKPlaceholder    = "SDK_PATH"
	MSVCPlaceholder   = "MSVC_PATH"
	ClangPlaceholder  = "CLANG_PATH"
)

// URIScheme prefixes every file URI clangd writes.
const URIScheme = "file:///"

// Roots names the absolute directories a machine substitutes for the four
// placeholders. An empty field means that root is unknown here: publishing
// treats an unknown root as an error only if URIs still reference it, and
// installing leaves the placeholder in place.
type Roots struct {
	Engine string
	SDK    string
	MSVC   string
	Clang  string
}

// RootBinding pairs a placeholder with the local path standing in for it.
type RootBinding struct {
	Placeholder string
	Path        string
}

// Each returns the bindings that are actually set, engine first. Longer
// paths sort before shorter ones so that a nested root wins over a parent.
func (r Roots) Each() []RootBinding {
	all := []RootBinding{
		{EnginePlaceholder, r.Engine},
		{SDKPlaceholder, r.SDK},
		{MSVCPlaceholder, r.MSVC},
		{ClangPlaceholder, r.Clang},
	}
	out := make([]RootBinding, 0, len(all))
	for _, b := range all {
		if b.Path != "" {
			out = append(out, b)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && len(out[j].Path) > len(out[j-1].Path); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func unreservedURIByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '_', c == '.', c == '~', c == '/', c == ':':
		return true
	}
	return false
}

const hexDigits = "0123456789ABCDEF"

// EncodeURI renders an absolute slash-separated path the way clangd does:
// every byte outside the unreserved set plus '/' and ':' is percent-encoded.
func EncodeURI(path string) string {
	var b strings.Builder
	b.Grow(len(URIScheme) + len(path))
	b.WriteString(URIScheme)
	for i := 0; i < len(path); i++ {
		c := path[i]
		if unreservedURIByte(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// DecodeURI strips the scheme and percent-decodes the path. A malformed
// escape is passed through literally rather than failing: this is used for
// prefix comparison, not for validation.
func DecodeURI(uri string) string {
	body := strings.TrimPrefix(uri, URIScheme)
	var b strings.Builder
	b.Grow(len(body))
	for i := 0; i < len(body); i++ {
		if body[i] == '%' && i+2 < len(body) {
			hi, okHi := unhex(body[i+1])
			lo, okLo := unhex(body[i+2])
			if okHi && okLo {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(body[i])
	}
	return b.String()
}

// DecodeURIPrefixLen returns how many bytes of uri encode the first
// decodedLen bytes of its decoded path, including the scheme. It returns -1
// when decodedLen does not fall on an encoded boundary, which callers treat
// as "this root is not a prefix of this URI".
func DecodeURIPrefixLen(uri string, decodedLen int) int {
	if !strings.HasPrefix(uri, URIScheme) {
		return -1
	}
	body := uri[len(URIScheme):]
	decoded := 0
	for i := 0; i < len(body); {
		if decoded == decodedLen {
			return len(URIScheme) + i
		}
		if body[i] == '%' && i+2 < len(body) {
			if _, okHi := unhex(body[i+1]); okHi {
				if _, okLo := unhex(body[i+2]); okLo {
					i += 3
					decoded++
					continue
				}
			}
		}
		i++
		decoded++
	}
	if decoded == decodedLen {
		return len(uri)
	}
	return -1
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/idxrewrite/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/idxrewrite/placeholder.go internal/idxrewrite/placeholder_test.go
git commit -m "feat(idxrewrite): clangd-compatible URI encoding and placeholder roots"
```

---

## Task 4: Normalize, Denormalize, and the absolute-path residue check

**Files:**
- Create: `internal/idxrewrite/rewrite.go`
- Test: `internal/idxrewrite/rewrite_test.go`

**Interfaces:**
- Consumes: Task 1 `Parse`/`Marshal`/`Find`, Task 2 `DecodeStringTable`/`EncodeStringTable`, Task 3 `Roots`/`EncodeURI`/`DecodeURI`/`DecodeURIPrefixLen`/placeholder constants.
- Produces: `type Result struct { Rewritten map[string]int; Unmatched int; UnmatchedSamples []string }`, `func Normalize(idx []byte, roots Roots) ([]byte, Result, error)`, `func Denormalize(idx []byte, roots Roots) ([]byte, Result, error)`, `func AbsolutePathResidue(idx []byte) (int, []string, error)`, `func IndexFormatVersion(idx []byte) (uint32, error)`.

Matching is case-insensitive because Windows paths are. Only the `stri` chunk changes; everything else is copied through, which is the invariant the tests assert directly.

- [ ] **Step 1: Write the failing test**

```go
package idxrewrite

import (
	"bytes"
	"testing"
)

func fixtureIndex(t *testing.T, entries ...string) []byte {
	t.Helper()
	c := &Container{FormType: FormType, Chunks: []Chunk{
		{ID: "meta", Data: []byte{20, 0, 0, 0}},
		{ID: StringTableChunkID, Data: EncodeStringTable(toBytes(entries))},
		{ID: "symb", Data: []byte("symbols-payload")},
		{ID: "refs", Data: []byte("refs-payload")},
		{ID: "rela", Data: []byte("rela")},
	}}
	return c.Marshal()
}

func toBytes(in []string) [][]byte {
	out := make([][]byte, len(in))
	for i, s := range in {
		out[i] = []byte(s)
	}
	return out
}

func tableOf(t *testing.T, idx []byte) []string {
	t.Helper()
	c, err := Parse(idx)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	chunk, ok := c.Find(StringTableChunkID)
	if !ok {
		t.Fatal("no string table")
	}
	entries, err := DecodeStringTable(chunk.Data)
	if err != nil {
		t.Fatalf("DecodeStringTable: %v", err)
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = string(e)
	}
	return out
}

func TestNormalizeReplacesEveryKnownRootWithItsPlaceholder(t *testing.T) {
	idx := fixtureIndex(t,
		"SomeSymbolName",
		"file:///D:/Elpis_UE5.8/Engine/Source/A.cpp",
		"file:///C:/Program%20Files%20%28x86%29/Windows%20Kits/10/um/w.h",
		"file:///C:/Program%20Files/Microsoft%20Visual%20Studio/VC/v.h",
		"file:///C:/tools/clangd/lib/clang/20/include/stdarg.h",
	)
	roots := Roots{
		Engine: "D:/Elpis_UE5.8",
		SDK:    "C:/Program Files (x86)/Windows Kits",
		MSVC:   "C:/Program Files/Microsoft Visual Studio",
		Clang:  "C:/tools/clangd/lib/clang/20",
	}
	out, res, err := Normalize(idx, roots)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if res.Unmatched != 0 {
		t.Fatalf("unmatched = %d (%q), want 0", res.Unmatched, res.UnmatchedSamples)
	}
	got := tableOf(t, out)
	want := []string{
		"SomeSymbolName",
		"file:///ENGINE_PATH/Engine/Source/A.cpp",
		"file:///SDK_PATH/10/um/w.h",
		"file:///MSVC_PATH/VC/v.h",
		"file:///CLANG_PATH/include/stdarg.h",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
	if res.Rewritten[EnginePlaceholder] != 1 {
		t.Fatalf("engine rewrites = %d, want 1", res.Rewritten[EnginePlaceholder])
	}
}

// Only the string table may change. If any other chunk moves or mutates,
// the index loads as garbage.
func TestNormalizeLeavesEveryOtherChunkByteIdentical(t *testing.T) {
	idx := fixtureIndex(t, "file:///D:/Engine/Source/A.cpp")
	out, _, err := Normalize(idx, Roots{Engine: "D:/Engine"})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	before, _ := Parse(idx)
	after, _ := Parse(out)
	if len(before.Chunks) != len(after.Chunks) {
		t.Fatalf("chunk count changed from %d to %d", len(before.Chunks), len(after.Chunks))
	}
	for i := range before.Chunks {
		if before.Chunks[i].ID != after.Chunks[i].ID {
			t.Fatalf("chunk %d id changed from %q to %q", i, before.Chunks[i].ID, after.Chunks[i].ID)
		}
		if before.Chunks[i].ID == StringTableChunkID {
			continue
		}
		if !bytes.Equal(before.Chunks[i].Data, after.Chunks[i].Data) {
			t.Fatalf("chunk %q changed", before.Chunks[i].ID)
		}
	}
}

// The strongest available check that the URI encoder matches clangd's: a
// normalise followed by a denormalise back to the same root must reproduce
// the original file exactly.
func TestNormalizeThenDenormalizeToSameRootIsIdentity(t *testing.T) {
	idx := fixtureIndex(t,
		"Sym",
		"file:///C:/Program%20Files%20%28x86%29/Windows%20Kits/10/um/w.h",
		"file:///D:/Elpis_UE5.8/Engine/Source/A.cpp",
	)
	roots := Roots{Engine: "D:/Elpis_UE5.8", SDK: "C:/Program Files (x86)/Windows Kits"}
	normalized, _, err := Normalize(idx, roots)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	back, _, err := Denormalize(normalized, roots)
	if err != nil {
		t.Fatalf("Denormalize: %v", err)
	}
	if !bytes.Equal(back, idx) {
		t.Fatalf("round trip through placeholders changed the index")
	}
}

func TestDenormalizeRetargetsToADifferentEngineRoot(t *testing.T) {
	idx := fixtureIndex(t, "file:///D:/Elpis_UE5.8/Engine/Source/A.cpp")
	normalized, _, err := Normalize(idx, Roots{Engine: "D:/Elpis_UE5.8"})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	out, res, err := Denormalize(normalized, Roots{Engine: "C:/UE_5.8"})
	if err != nil {
		t.Fatalf("Denormalize: %v", err)
	}
	if got, want := tableOf(t, out)[0], "file:///C:/UE_5.8/Engine/Source/A.cpp"; got != want {
		t.Fatalf("entry = %q, want %q", got, want)
	}
	if res.Rewritten[EnginePlaceholder] != 1 {
		t.Fatalf("engine rewrites = %d, want 1", res.Rewritten[EnginePlaceholder])
	}
}

// An unknown root must be reported, not silently published. This is what
// makes the "no absolute paths in a published artifact" invariant real.
func TestNormalizeReportsURIsUnderNoKnownRoot(t *testing.T) {
	idx := fixtureIndex(t, "file:///E:/ThirdParty/sdk/x.h")
	_, res, err := Normalize(idx, Roots{Engine: "D:/Elpis_UE5.8"})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if res.Unmatched != 1 {
		t.Fatalf("unmatched = %d, want 1", res.Unmatched)
	}
	if len(res.UnmatchedSamples) == 0 {
		t.Fatal("UnmatchedSamples is empty; the publish error would name nothing")
	}
}

func TestAbsolutePathResidueCountsRemainingDriveLetterPaths(t *testing.T) {
	clean := fixtureIndex(t, "Sym", "file:///ENGINE_PATH/Engine/A.cpp")
	n, samples, err := AbsolutePathResidue(clean)
	if err != nil {
		t.Fatalf("AbsolutePathResidue: %v", err)
	}
	if n != 0 {
		t.Fatalf("residue = %d (%q), want 0", n, samples)
	}
	dirty := fixtureIndex(t, "file:///C:/Users/someone/x.h")
	n, samples, err = AbsolutePathResidue(dirty)
	if err != nil {
		t.Fatalf("AbsolutePathResidue: %v", err)
	}
	if n != 1 || len(samples) != 1 {
		t.Fatalf("residue = %d samples = %q, want 1 and one sample", n, samples)
	}
}

func TestNormalizeMatchesRootsCaseInsensitively(t *testing.T) {
	idx := fixtureIndex(t, "file:///D:/Elpis_UE5.8/Engine/A.cpp")
	out, res, err := Normalize(idx, Roots{Engine: "d:/elpis_ue5.8"})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if res.Rewritten[EnginePlaceholder] != 1 {
		t.Fatalf("rewrites = %d, want 1; Windows paths differ only in case", res.Rewritten[EnginePlaceholder])
	}
	if got, want := tableOf(t, out)[0], "file:///ENGINE_PATH/Engine/A.cpp"; got != want {
		t.Fatalf("entry = %q, want %q", got, want)
	}
}

func TestIndexFormatVersionReadsMetaChunk(t *testing.T) {
	got, err := IndexFormatVersion(fixtureIndex(t, "Sym"))
	if err != nil {
		t.Fatalf("IndexFormatVersion: %v", err)
	}
	if got != 20 {
		t.Fatalf("version = %d, want 20", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/idxrewrite/... -run 'Normalize|Denormalize|Residue|FormatVersion' -v`
Expected: FAIL to compile, `undefined: Normalize`.

- [ ] **Step 3: Write minimal implementation**

```go
package idxrewrite

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// MetaChunkID holds the index format version clangd wrote.
const MetaChunkID = "meta"

const unmatchedSampleLimit = 8

// Result reports what a rewrite did. Unmatched counts file URIs that fell
// under no known root: publishing must refuse while any remain, because
// each one is an absolute local path that would ship in the artifact.
type Result struct {
	Rewritten        map[string]int
	Unmatched        int
	UnmatchedSamples []string
}

// IndexFormatVersion reads the meta chunk. It is compared against the value
// a manifest claims, so that a corrupted or mislabelled artifact is refused
// rather than handed to clangd.
func IndexFormatVersion(idx []byte) (uint32, error) {
	c, err := Parse(idx)
	if err != nil {
		return 0, err
	}
	meta, ok := c.Find(MetaChunkID)
	if !ok || len(meta.Data) < 4 {
		return 0, fmt.Errorf("%w: missing or short meta chunk", ErrNotAnIndex)
	}
	return binary.LittleEndian.Uint32(meta.Data[0:4]), nil
}

// Normalize replaces each known absolute root with its placeholder.
func Normalize(idx []byte, roots Roots) ([]byte, Result, error) {
	return rewrite(idx, func(uri string) (string, string, bool) {
		decoded := DecodeURI(uri)
		lowered := strings.ToLower(decoded)
		for _, b := range roots.Each() {
			root := strings.TrimRight(strings.ToLower(b.Path), "/")
			if !strings.HasPrefix(lowered, root) {
				continue
			}
			if len(decoded) > len(root) && decoded[len(root)] != '/' {
				continue
			}
			cut := DecodeURIPrefixLen(uri, len(root))
			if cut < 0 {
				continue
			}
			return URIScheme + b.Placeholder + uri[cut:], b.Placeholder, true
		}
		return "", "", false
	})
}

// Denormalize replaces each placeholder with the local root standing in for
// it. A placeholder with no local root configured is left in place: the
// symbols behind it simply do not resolve, which is a far better outcome
// than an index that points at a path this machine does not have.
func Denormalize(idx []byte, roots Roots) ([]byte, Result, error) {
	return rewrite(idx, func(uri string) (string, string, bool) {
		for _, b := range roots.Each() {
			prefix := URIScheme + b.Placeholder
			if !strings.HasPrefix(uri, prefix) {
				continue
			}
			if len(uri) > len(prefix) && uri[len(prefix)] != '/' {
				continue
			}
			return EncodeURI(strings.TrimRight(b.Path, "/")) + uri[len(prefix):], b.Placeholder, true
		}
		return "", "", false
	})
}

// rewrite applies fn to every file URI in the string table and re-emits the
// container. Only the string table is rebuilt; every other chunk is carried
// through untouched, because they address strings by index and are
// unaffected by a string's length changing.
func rewrite(idx []byte, fn func(uri string) (string, string, bool)) ([]byte, Result, error) {
	res := Result{Rewritten: map[string]int{}}
	c, err := Parse(idx)
	if err != nil {
		return nil, res, err
	}
	chunk, ok := c.Find(StringTableChunkID)
	if !ok {
		return nil, res, fmt.Errorf("%w: missing string table", ErrNotAnIndex)
	}
	entries, err := DecodeStringTable(chunk.Data)
	if err != nil {
		return nil, res, err
	}
	out := make([][]byte, len(entries))
	for i, e := range entries {
		s := string(e)
		if !strings.HasPrefix(s, URIScheme) {
			out[i] = e
			continue
		}
		replaced, placeholder, matched := fn(s)
		if !matched {
			out[i] = e
			res.Unmatched++
			if len(res.UnmatchedSamples) < unmatchedSampleLimit {
				res.UnmatchedSamples = append(res.UnmatchedSamples, s)
			}
			continue
		}
		out[i] = []byte(replaced)
		res.Rewritten[placeholder]++
	}
	chunk.Data = EncodeStringTable(out)
	return c.Marshal(), res, nil
}

// AbsolutePathResidue counts string-table entries that still contain a
// drive-letter path. Publishing asserts this is zero, which is what makes
// "a published artifact carries no local paths" a checkable property rather
// than an intention.
func AbsolutePathResidue(idx []byte) (int, []string, error) {
	c, err := Parse(idx)
	if err != nil {
		return 0, nil, err
	}
	chunk, ok := c.Find(StringTableChunkID)
	if !ok {
		return 0, nil, fmt.Errorf("%w: missing string table", ErrNotAnIndex)
	}
	entries, err := DecodeStringTable(chunk.Data)
	if err != nil {
		return 0, nil, err
	}
	count := 0
	var samples []string
	for _, e := range entries {
		if !HasAbsolutePath(string(e)) {
			continue
		}
		count++
		if len(samples) < unmatchedSampleLimit {
			samples = append(samples, string(e))
		}
	}
	return count, samples, nil
}

// AbsolutePathIndex returns the offset of the first unambiguously absolute
// path in s, or -1. Only drive-letter and UNC forms count here, in raw and
// percent-encoded spelling, because those cannot be confused with anything
// else. A leading "/" is deliberately excluded: clang-cl spells flags that
// way, and treating "/nologo" as a path would reject every real database.
func AbsolutePathIndex(s string) int {
	for i := 0; i < len(s); i++ {
		if at := absoluteAt(s, i); at {
			return i
		}
	}
	return -1
}

func absoluteAt(s string, i int) bool {
	c := s[i]
	if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
		// C:/x or C:\x
		if i+2 < len(s) && s[i+1] == ':' && (s[i+2] == '/' || s[i+2] == '\\') {
			return true
		}
		// C%3A/x, as a URI would spell it
		if i+5 < len(s) && s[i+1] == '%' && s[i+2] == '3' &&
			(s[i+3] == 'A' || s[i+3] == 'a') && (s[i+4] == '/' || s[i+4] == '\\') {
			return true
		}
	}
	// UNC: \\server\share or //server/share, but not the "//" inside a URI
	// scheme, which is why the preceding byte must not be a colon.
	if i+2 < len(s) && (c == '\\' || c == '/') && s[i+1] == c && s[i+2] != c {
		if i == 0 || s[i-1] != ':' {
			return true
		}
	}
	return false
}

// PlaceholderResidue counts string-table entries still carrying the given
// placeholder. Install asserts this is zero for ENGINE_PATH. It decodes the
// table rather than scanning the whole file: converting a 539 MB index to a
// string doubles peak memory and would also match placeholder bytes that
// merely occur inside refs or symb.
func PlaceholderResidue(idx []byte, placeholder string) (int, error) {
	c, err := Parse(idx)
	if err != nil {
		return 0, err
	}
	chunk, ok := c.Find(StringTableChunkID)
	if !ok {
		return 0, fmt.Errorf("%w: missing string table", ErrNotAnIndex)
	}
	entries, err := DecodeStringTable(chunk.Data)
	if err != nil {
		return 0, err
	}
	prefix := URIScheme + placeholder
	count := 0
	for _, e := range entries {
		if strings.HasPrefix(string(e), prefix) {
			count++
		}
	}
	return count, nil
}

// HasAbsolutePath reports whether s is or contains an absolute path. It is
// stricter than AbsolutePathIndex: it also treats a POSIX rooted path as
// absolute. Use it on values that are known to be paths -- a flag's value, a
// source operand, a string-table entry -- never on an arbitrary token, where
// a clang-cl flag would trip it.
func HasAbsolutePath(s string) bool {
	if AbsolutePathIndex(s) >= 0 {
		return true
	}
	if strings.HasPrefix(s, "/") && strings.Count(s, "/") > 1 {
		return true
	}
	return false
}
```

`rewrite.go` needs `"strings"` in its imports.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/idxrewrite/... -v`
Expected: PASS, all tests across tasks 1 to 4.

- [ ] **Step 5: Commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/idxrewrite/rewrite.go internal/idxrewrite/rewrite_test.go
git commit -m "feat(idxrewrite): normalize and denormalize index roots with a residue check"
```

---

## Task 5: Build.version parsing

**Files:**
- Create: `internal/engineid/buildversion.go`
- Test: `internal/engineid/buildversion_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `type BuildVersion struct { MajorVersion, MinorVersion, PatchVersion int; Changelist, CompatibleChangelist int64; IsLicenseeVersion, IsPromotedBuild int; BranchName, BuildID string }`, `func ReadBuildVersion(engineRoot string) (BuildVersion, error)`, `func (v BuildVersion) Label() string`.

`internal/unreal.ValidateEngine` already reads this file but keeps only major and minor. This package needs every field, so it parses independently rather than widening the existing type, which several callers depend on.

Note the JSON key is `BuildId`, not `BuildID`; the Go field is named `BuildID` for lint cleanliness and carries an explicit tag.

- [ ] **Step 1: Write the failing test**

```go
package engineid

import (
	"os"
	"path/filepath"
	"testing"
)

func writeBuildVersion(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "Engine", "Build")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Build.version"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return root
}

func TestReadBuildVersionParsesEveryIdentityField(t *testing.T) {
	root := writeBuildVersion(t, `{
		"MajorVersion": 5, "MinorVersion": 8, "PatchVersion": 1,
		"Changelist": 0, "CompatibleChangelist": 55116800,
		"IsLicenseeVersion": 0, "IsPromotedBuild": 0,
		"BranchName": "UE5", "BuildId": "custom-5.8"
	}`)
	got, err := ReadBuildVersion(root)
	if err != nil {
		t.Fatalf("ReadBuildVersion: %v", err)
	}
	if got.PatchVersion != 1 {
		t.Fatalf("patch = %d, want 1; dropping it collides 5.8.0 with 5.8.1", got.PatchVersion)
	}
	if got.CompatibleChangelist != 55116800 {
		t.Fatalf("compatible changelist = %d, want 55116800", got.CompatibleChangelist)
	}
	if got.BranchName != "UE5" || got.BuildID != "custom-5.8" {
		t.Fatalf("branch/build id = %q/%q", got.BranchName, got.BuildID)
	}
}

func TestLabelIsHumanReadableAndVersionBearing(t *testing.T) {
	v := BuildVersion{MajorVersion: 5, MinorVersion: 8, PatchVersion: 1, CompatibleChangelist: 55116800, BranchName: "UE5"}
	if got, want := v.Label(), "5.8.1-cl55116800-UE5"; got != want {
		t.Fatalf("Label = %q, want %q", got, want)
	}
}

func TestLabelOmitsAnEmptyBranch(t *testing.T) {
	v := BuildVersion{MajorVersion: 5, MinorVersion: 8, PatchVersion: 0, CompatibleChangelist: 1}
	if got, want := v.Label(), "5.8.0-cl1"; got != want {
		t.Fatalf("Label = %q, want %q", got, want)
	}
}

func TestReadBuildVersionReportsAMissingFile(t *testing.T) {
	if _, err := ReadBuildVersion(t.TempDir()); err == nil {
		t.Fatal("ReadBuildVersion accepted a root with no Build.version")
	}
}

func TestReadBuildVersionReportsMalformedJSON(t *testing.T) {
	if _, err := ReadBuildVersion(writeBuildVersion(t, "{not json")); err == nil {
		t.Fatal("ReadBuildVersion accepted malformed JSON")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engineid/... -v`
Expected: FAIL to compile, `undefined: ReadBuildVersion`.

- [ ] **Step 3: Write minimal implementation**

```go
// Package engineid derives a path-independent identity for an engine
// install, so that a prebuilt index built on one machine can be matched to
// the same engine on another.
//
// The existing engineindex.Key deliberately hashes the engine root path and
// local absolute file paths, which is correct for a local cache and useless
// for sharing: two people with the same engine at different paths compute
// different keys and could never match. This package computes the other
// half of that pair.
package engineid

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BuildVersion is Engine/Build/Build.version in full. unreal.ValidateEngine
// reads the same file but keeps only major and minor, which is too coarse
// to tell two builds of one minor version apart.
type BuildVersion struct {
	MajorVersion         int    `json:"MajorVersion"`
	MinorVersion         int    `json:"MinorVersion"`
	PatchVersion         int    `json:"PatchVersion"`
	Changelist           int64  `json:"Changelist"`
	CompatibleChangelist int64  `json:"CompatibleChangelist"`
	IsLicenseeVersion    int    `json:"IsLicenseeVersion"`
	IsPromotedBuild      int    `json:"IsPromotedBuild"`
	BranchName           string `json:"BranchName"`
	BuildID              string `json:"BuildId"`
}

// ReadBuildVersion parses the engine's version marker.
func ReadBuildVersion(engineRoot string) (BuildVersion, error) {
	path := filepath.Join(engineRoot, "Engine", "Build", "Build.version")
	raw, err := os.ReadFile(path)
	if err != nil {
		return BuildVersion{}, fmt.Errorf("read Build.version: %w", err)
	}
	var v BuildVersion
	if err := json.Unmarshal(raw, &v); err != nil {
		return BuildVersion{}, fmt.Errorf("decode Build.version: %w", err)
	}
	return v, nil
}

// Label renders the human-readable part of an engine identity.
func (v BuildVersion) Label() string {
	label := fmt.Sprintf("%d.%d.%d-cl%d", v.MajorVersion, v.MinorVersion, v.PatchVersion, v.CompatibleChangelist)
	if branch := strings.TrimSpace(v.BranchName); branch != "" {
		label += "-" + branch
	}
	return label
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/engineid/... -v`
Expected: PASS, 5 tests.

- [ ] **Step 5: Commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/engineid/buildversion.go internal/engineid/buildversion_test.go
git commit -m "feat(engineid): parse the full Build.version identity marker"
```

---

## Task 6: Response-file expansion and compile-argument path substitution

**Files:**
- Create: `internal/engineid/argpaths.go`
- Test: `internal/engineid/argpaths_test.go`

**Interfaces:**
- Consumes: `idxrewrite.Roots`, `idxrewrite.HasAbsolutePath`, placeholder constants, `compdb.Entry`.
- Produces: `var ErrAbsolutePathInArgs error`, `var ErrDanglingFlagValue error`, `func TokenizeResponseFile(body string) []string`, `func ExpandArguments(entry compdb.Entry, read func(path string) (string, error)) ([]string, string, error)`, `func NormalizeArgs(args []string, roots idxrewrite.Roots) ([]string, error)`.

### What the real data looks like

Checked against this project's own generated database — 26,782 entries, all of the same shape:

```json
{"directory": "<project or plugin dir>",
 "file":      "<absolute source path>",
 "arguments": ["C:\\Program Files\\LLVM\\bin\\clang-cl.exe",
               "@C:\\Users\\<user>\\AppData\\Local\\soft-ue-index\\projects\\<id>\\responses\\<sha256>.rsp",
               "<absolute source path>"]}
```

Three facts that decide this task's shape:

1. **The flags are not in `arguments`.** They live in a response file, 1,234 distinct ones here, about 62 KB each. Normalising only `arguments` sees no include paths at all.
2. **`arguments[0]` is the compiler**, an absolute path under none of the four roots. Treating it as a path value fails every entry.
3. **Response files contain quoted values with spaces**, for example `/I"C:\Program Files\...\MSVC\...\INCLUDE"`. Splitting on whitespace corrupts them.

So expansion comes first, then substitution. `ExpandArguments` returns the expanded flag list *and* the compiler's base name separately; the install path of the compiler never enters the identity, only its identity does, so two machines with clang-cl in different locations still agree.

A token that still holds an absolute path after substitution is a **hard error**. Passing it through is the tempting default and it is exactly wrong: one stray absolute path silently makes the key machine-specific, and it fails invisibly rather than loudly.

The residue check runs over the value-bearing portion of **every** token, including joined flags this table does not know. An unknown flag that happens to carry a path — `-fmodule-map-file=D:/x`, `/FoD:/x`, `/winsysrootD:/x` — must not escape by virtue of starting with `-` or `/`.

- [ ] **Step 1: Write the failing test**

```go
package engineid

import (
	"errors"
	"strings"
	"testing"

	"github.com/softdaddy-o/soft-ue-index/internal/idxrewrite"
)

func testRoots() idxrewrite.Roots {
	return idxrewrite.Roots{
		Engine: "D:/Elpis_UE5.8",
		SDK:    "C:/Program Files (x86)/Windows Kits",
		MSVC:   "C:/Program Files/Microsoft Visual Studio",
		Clang:  "C:/tools/clangd/lib/clang/20",
	}
}

func TestNormalizeArgsSubstitutesJoinedFlagValues(t *testing.T) {
	got, err := NormalizeArgs([]string{`-ID:/Elpis_UE5.8/Engine/Source`, "-DWITH_EDITOR=1"}, testRoots())
	if err != nil {
		t.Fatalf("NormalizeArgs: %v", err)
	}
	want := []string{"-IENGINE_PATH/Engine/Source", "-DWITH_EDITOR=1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeArgsSubstitutesSeparatedFlagValues(t *testing.T) {
	got, err := NormalizeArgs([]string{"-isystem", `C:/Program Files (x86)/Windows Kits/10/um`}, testRoots())
	if err != nil {
		t.Fatalf("NormalizeArgs: %v", err)
	}
	want := []string{"-isystem", "SDK_PATH/10/um"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeArgsHandlesClangClAndEqualsForms(t *testing.T) {
	got, err := NormalizeArgs([]string{
		`/ID:/Elpis_UE5.8/Engine/Source`,
		`/external:IC:/Program Files/Microsoft Visual Studio/VC/include`,
		`-fmodules-cache-path=D:/Elpis_UE5.8/Intermediate/mods`,
		`--sysroot=D:/Elpis_UE5.8`,
	}, testRoots())
	if err != nil {
		t.Fatalf("NormalizeArgs: %v", err)
	}
	want := []string{
		"/IENGINE_PATH/Engine/Source",
		"/external:IMSVC_PATH/VC/include",
		"-fmodules-cache-path=ENGINE_PATH/Intermediate/mods",
		"--sysroot=ENGINE_PATH",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeArgsSubstitutesTheBareSourceOperand(t *testing.T) {
	got, err := NormalizeArgs([]string{"-c", `D:/Elpis_UE5.8/Engine/Source/A.cpp`}, testRoots())
	if err != nil {
		t.Fatalf("NormalizeArgs: %v", err)
	}
	if got[1] != "ENGINE_PATH/Engine/Source/A.cpp" {
		t.Fatalf("operand = %q", got[1])
	}
}

func TestNormalizeArgsAcceptsBackslashPaths(t *testing.T) {
	got, err := NormalizeArgs([]string{`-ID:\Elpis_UE5.8\Engine\Source`}, testRoots())
	if err != nil {
		t.Fatalf("NormalizeArgs: %v", err)
	}
	if got[0] != "-IENGINE_PATH/Engine/Source" {
		t.Fatalf("got %q, want -IENGINE_PATH/Engine/Source", got[0])
	}
}

// The whole point of the identity layer. A path that survives substitution
// must stop the computation, not quietly poison the hash.
func TestNormalizeArgsRejectsAnUnrecognisedAbsolutePath(t *testing.T) {
	_, err := NormalizeArgs([]string{`-IE:/SomeOtherSDK/include`}, testRoots())
	if !errors.Is(err, ErrAbsolutePathInArgs) {
		t.Fatalf("err = %v, want ErrAbsolutePathInArgs", err)
	}
}

func TestNormalizeArgsRejectsAnAbsoluteBareOperandUnderNoRoot(t *testing.T) {
	_, err := NormalizeArgs([]string{`E:/scratch/A.cpp`}, testRoots())
	if !errors.Is(err, ErrAbsolutePathInArgs) {
		t.Fatalf("err = %v, want ErrAbsolutePathInArgs", err)
	}
}

// A flag name is never a path, even when it looks like one.
func TestNormalizeArgsDoesNotTreatClangClFlagsAsPaths(t *testing.T) {
	got, err := NormalizeArgs([]string{"/std:c++20", "/W4", "/EHsc"}, testRoots())
	if err != nil {
		t.Fatalf("NormalizeArgs: %v", err)
	}
	if strings.Join(got, "|") != "/std:c++20|/W4|/EHsc" {
		t.Fatalf("got %q", got)
	}
}

func TestNormalizeArgsIsCaseInsensitiveOnRoots(t *testing.T) {
	got, err := NormalizeArgs([]string{`-Id:/elpis_ue5.8/Engine`}, testRoots())
	if err != nil {
		t.Fatalf("NormalizeArgs: %v", err)
	}
	if got[0] != "-IENGINE_PATH/Engine" {
		t.Fatalf("got %q", got[0])
	}
}

// Real response files quote values that contain spaces. Splitting on
// whitespace corrupts every MSVC and Windows Kits include path.
func TestTokenizeResponseFileKeepsQuotedValuesIntact(t *testing.T) {
	body := "/nologo\n/std:c++20\n" +
		`/I"C:\Program Files\Microsoft Visual Studio\18\VC\INCLUDE"` + "\n" +
		`/ID:\Elpis_UE5.8\Engine\Source` + "\n"
	got := TokenizeResponseFile(body)
	want := []string{"/nologo", "/std:c++20",
		`/IC:\Program Files\Microsoft Visual Studio\18\VC\INCLUDE`,
		`/ID:\Elpis_UE5.8\Engine\Source`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTokenizeResponseFileSplitsSpaceSeparatedTokens(t *testing.T) {
	got := TokenizeResponseFile(`-DA=1 -DB=2   -isystem "C:/a b/inc"`)
	want := []string{"-DA=1", "-DB=2", "-isystem", "C:/a b/inc"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// arguments[0] is the compiler. Its install path must not reach the identity,
// or two machines with clang-cl in different places never match.
func TestExpandArgumentsSeparatesTheCompilerFromTheFlags(t *testing.T) {
	entry := compdb.Entry{
		Directory: `D:/Elpis_UE5.8`,
		File:      `D:/Elpis_UE5.8/Engine/Source/A.cpp`,
		Arguments: []string{
			`C:\Program Files\LLVM\bin\clang-cl.exe`,
			`@C:\Users\someone\responses\abc.rsp`,
			`D:/Elpis_UE5.8/Engine/Source/A.cpp`,
		},
	}
	read := func(path string) (string, error) {
		if !strings.HasSuffix(path, "abc.rsp") {
			t.Fatalf("unexpected response file %q", path)
		}
		return "/std:c++20\n" + `/ID:\Elpis_UE5.8\Engine\Source` + "\n", nil
	}
	args, compiler, err := ExpandArguments(entry, read)
	if err != nil {
		t.Fatalf("ExpandArguments: %v", err)
	}
	if compiler != "clang-cl.exe" {
		t.Fatalf("compiler = %q, want the base name only", compiler)
	}
	for _, a := range args {
		if strings.Contains(a, "LLVM") || strings.Contains(a, ".rsp") {
			t.Fatalf("expanded args still carry the compiler or response path: %q", args)
		}
	}
	if strings.Join(args, "|") != `/std:c++20|/ID:\Elpis_UE5.8\Engine\Source|D:/Elpis_UE5.8/Engine/Source/A.cpp` {
		t.Fatalf("args = %q", args)
	}
}

func TestExpandArgumentsReportsAnUnreadableResponseFile(t *testing.T) {
	entry := compdb.Entry{Arguments: []string{"clang-cl.exe", "@missing.rsp", "a.cpp"}}
	read := func(string) (string, error) { return "", errors.New("nope") }
	if _, _, err := ExpandArguments(entry, read); err == nil {
		t.Fatal("ExpandArguments ignored an unreadable response file")
	}
}

func TestExpandArgumentsHandlesEntriesWithInlineFlags(t *testing.T) {
	entry := compdb.Entry{Arguments: []string{"clang++", "-DX=1", "a.cpp"}}
	args, compiler, err := ExpandArguments(entry, func(string) (string, error) {
		t.Fatal("no response file should be read")
		return "", nil
	})
	if err != nil {
		t.Fatalf("ExpandArguments: %v", err)
	}
	if compiler != "clang++" || strings.Join(args, "|") != "-DX=1|a.cpp" {
		t.Fatalf("args = %q compiler = %q", args, compiler)
	}
}

// An unknown joined flag must not smuggle a path through just because it
// begins with - or /.
func TestNormalizeArgsRejectsAPathInsideAnUnknownJoinedFlag(t *testing.T) {
	for _, arg := range []string{
		`-fmodule-map-file=E:/other/module.modulemap`,
		`/FoE:/other/out.obj`,
		`/winsysrootE:/other`,
		`-ivfsoverlayE:/other/vfs.yaml`,
	} {
		if _, err := NormalizeArgs([]string{arg}, testRoots()); !errors.Is(err, ErrAbsolutePathInArgs) {
			t.Fatalf("NormalizeArgs(%q) err = %v, want ErrAbsolutePathInArgs", arg, err)
		}
	}
}

func TestNormalizeArgsSubstitutesAKnownPathInsideAnUnknownJoinedFlag(t *testing.T) {
	got, err := NormalizeArgs([]string{`-fmodule-map-file=D:/Elpis_UE5.8/Engine/m.modulemap`}, testRoots())
	if err != nil {
		t.Fatalf("NormalizeArgs: %v", err)
	}
	if got[0] != "-fmodule-map-file=ENGINE_PATH/Engine/m.modulemap" {
		t.Fatalf("got %q", got[0])
	}
}

// -include-pch is not -include; prefix matching alone mis-parses it.
func TestNormalizeArgsDoesNotMistakeALongerFlagForAShorterOne(t *testing.T) {
	got, err := NormalizeArgs([]string{"-include-pch", `D:/Elpis_UE5.8/Engine/a.pch`}, testRoots())
	if err != nil {
		t.Fatalf("NormalizeArgs: %v", err)
	}
	if strings.Join(got, "|") != "-include-pch|ENGINE_PATH/Engine/a.pch" {
		t.Fatalf("got %q", got)
	}
}

func TestNormalizeArgsRejectsADanglingValueFlag(t *testing.T) {
	if _, err := NormalizeArgs([]string{"-isystem"}, testRoots()); !errors.Is(err, ErrDanglingFlagValue) {
		t.Fatalf("err = %v, want ErrDanglingFlagValue", err)
	}
}

func TestNormalizeArgsRejectsAUNCPath(t *testing.T) {
	if _, err := NormalizeArgs([]string{`-I\\server\share\inc`}, testRoots()); !errors.Is(err, ErrAbsolutePathInArgs) {
		t.Fatalf("err = %v, want ErrAbsolutePathInArgs", err)
	}
}
```

Add `"github.com/softdaddy-o/soft-ue-index/internal/compdb"` to this file's import block.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engineid/... -run NormalizeArgs -v`
Expected: FAIL to compile, `undefined: NormalizeArgs`.

- [ ] **Step 3: Write minimal implementation**

```go
package engineid

import (
	"errors"
	"fmt"
	"strings"

	"github.com/softdaddy-o/soft-ue-index/internal/idxrewrite"
)

// ErrAbsolutePathInArgs reports a compile-argument token that still held an
// absolute path after substitution. This is fatal on purpose: an identity
// computed from such a token is machine-specific, which is exactly what the
// identity layer exists to prevent, and it would fail silently.
var ErrAbsolutePathInArgs = errors.New("unrecognised absolute path in compile arguments")

// ErrDanglingFlagValue reports a value-taking flag with nothing after it.
// Silently succeeding there would hash a truncated argument list.
var ErrDanglingFlagValue = errors.New("compile argument flag is missing its value")

// separatedValueFlags take their path as the following argument. Matched by
// exact spelling, never by prefix, so that -include-pch is not read as
// -include followed by "-pch".
var separatedValueFlags = map[string]bool{
	"-I": true, "-isystem": true, "-imsvc": true, "-iquote": true,
	"-idirafter": true, "-isysroot": true, "--sysroot": true,
	"-include": true, "-include-pch": true, "-ivfsoverlay": true,
	"-fmodule-map-file": true, "/I": true, "/FI": true, "/Fo": true, "/Fp": true,
}

// joinedValuePrefixes carry their value in the same token, longest first so
// that /external:I wins over /I and -isystem over -I.
var joinedValuePrefixes = []string{
	"-fmodules-cache-path=", "-fmodule-map-file=", "-fmodule-file=",
	"-ivfsoverlay", "-include-pch", "--sysroot=", "/external:I",
	"-idirafter", "-winsysroot", "/winsysroot", "-isysroot",
	"-isystem", "-imsvc", "-iquote", "-include", "-I", "/I", "/FI", "/Fo", "/Fp",
}

// TokenizeResponseFile splits a clang response file into arguments. Values are
// commonly quoted because they contain spaces -- this project's own response
// files quote every MSVC and Windows Kits include path -- so splitting on
// whitespace alone corrupts them.
func TokenizeResponseFile(body string) []string {
	var out []string
	var cur strings.Builder
	inQuote, started := false, false
	flush := func() {
		if started {
			out = append(out, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			started = true
		case !inQuote && (c == ' ' || c == '\t' || c == '\r' || c == '\n'):
			flush()
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	flush()
	return out
}

// ExpandArguments resolves one database entry into the flags that actually
// applied, plus the compiler's base name.
//
// The real database this feature targets stores nothing but
// [compiler, @response-file, source]: the flags live in the response file, so
// an implementation that reads only Arguments sees no include paths at all.
// The compiler is returned separately and by base name only, because its
// install path is machine-specific and must never reach the identity.
func ExpandArguments(entry compdb.Entry, read func(path string) (string, error)) ([]string, string, error) {
	args := entry.Arguments
	if len(args) == 0 {
		return nil, "", fmt.Errorf("entry for %q has no arguments; a command string cannot be tokenised reliably across platforms", entry.File)
	}
	compiler := filepath.Base(strings.ReplaceAll(args[0], `\`, "/"))
	out := make([]string, 0, len(args))
	for _, arg := range args[1:] {
		if !strings.HasPrefix(arg, "@") {
			out = append(out, arg)
			continue
		}
		body, err := read(arg[1:])
		if err != nil {
			return nil, "", fmt.Errorf("expand response file %s: %w", arg[1:], err)
		}
		out = append(out, TokenizeResponseFile(body)...)
	}
	return out, compiler, nil
}

// NormalizeArgs substitutes every path-bearing argument value with the
// placeholder for the root containing it, leaving flag names untouched.
func NormalizeArgs(args []string, roots idxrewrite.Roots) ([]string, error) {
	out := make([]string, 0, len(args))
	pending := ""
	for _, arg := range args {
		if pending != "" {
			value, err := substitutePath(arg, roots)
			if err != nil {
				return nil, err
			}
			pending = ""
			out = append(out, value)
			continue
		}
		if separatedValueFlags[arg] {
			pending = arg
			out = append(out, arg)
			continue
		}
		if prefix, rest, ok := splitJoined(arg); ok {
			value, err := substitutePath(rest, roots)
			if err != nil {
				return nil, err
			}
			out = append(out, prefix+value)
			continue
		}
		value, err := substituteEmbedded(arg, roots)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	if pending != "" {
		return nil, fmt.Errorf("%w: %s", ErrDanglingFlagValue, pending)
	}
	return out, nil
}

// splitJoined finds the longest matching joined-value prefix.
func splitJoined(arg string) (prefix, rest string, ok bool) {
	best := ""
	for _, p := range joinedValuePrefixes {
		if len(p) > len(best) && len(arg) > len(p) && strings.HasPrefix(arg, p) {
			best = p
		}
	}
	if best == "" {
		return "", "", false
	}
	return best, arg[len(best):], true
}

// substituteEmbedded handles a token this table does not recognise. A flag
// name alone is harmless, but an unknown joined flag can still carry a path
// -- /FoD:/out.obj, -fmodule-map-file=D:/x -- so the value-bearing tail is
// substituted and then residue-checked rather than waved through because the
// token happens to start with - or /.
func substituteEmbedded(arg string, roots idxrewrite.Roots) (string, error) {
	slashed := strings.ReplaceAll(arg, `\`, "/")
	at := idxrewrite.AbsolutePathIndex(slashed)
	if at < 0 {
		return slashed, nil
	}
	head, tail := slashed[:at], slashed[at:]
	value, err := substitutePath(tail, roots)
	if err != nil {
		return "", err
	}
	return head + value, nil
}

// substitutePath rewrites one path value, erroring if it is absolute and
// under no known root.
func substitutePath(value string, roots idxrewrite.Roots) (string, error) {
	slashed := strings.ReplaceAll(value, `\`, "/")
	lowered := strings.ToLower(slashed)
	for _, b := range roots.Each() {
		root := strings.ToLower(strings.TrimRight(strings.ReplaceAll(b.Path, `\`, "/"), "/"))
		if root == "" || !strings.HasPrefix(lowered, root) {
			continue
		}
		if len(slashed) > len(root) && slashed[len(root)] != '/' {
			continue
		}
		return b.Placeholder + slashed[len(root):], nil
	}
	if idxrewrite.HasAbsolutePath(slashed) {
		return "", fmt.Errorf("%w: %s", ErrAbsolutePathInArgs, value)
	}
	return slashed, nil
}
```

Imports for this file:

```go
import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/softdaddy-o/soft-ue-index/internal/compdb"
	"github.com/softdaddy-o/soft-ue-index/internal/idxrewrite"
)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/engineid/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/engineid/argpaths.go internal/engineid/argpaths_test.go
git commit -m "feat(engineid): substitute path-bearing compile arguments, fail on residue"
```

---

## Task 7: Structural hash

**Files:**
- Create: `internal/engineid/structural.go`
- Test: `internal/engineid/structural_test.go`

**Interfaces:**
- Consumes: Task 5 `BuildVersion`, Task 6 `NormalizeArgs`, `compdb.Entry`, `idxrewrite.Roots`.
- Produces: `type ResponseReader func(path string) (string, error)`, `func CachingResponseReader() ResponseReader`, `func StructuralHash(engineRoot string, entries []compdb.Entry, v BuildVersion, roots idxrewrite.Roots, read ResponseReader) (string, error)`.

Every input is pinned here, because two implementations that differ on any of them never match each other:

- Paths are made relative to the engine root, separators normalised to `/`, lowercased, deduplicated, sorted by byte order.
- Arguments keep their order within an entry; entries are ordered by normalised relative path, then by joined arguments.
- `BuildID` and `IsLicenseeVersion` are folded into this hash rather than the readable label, so the label stays short while the identity still separates two builds that differ only in those fields.
- Output is 16 lowercase hex characters, matching `engineindex.Key`'s convention.

- [ ] **Step 1: Write the failing test**

```go
package engineid

import (
	"testing"

	"github.com/softdaddy-o/soft-ue-index/internal/compdb"
)

// entry mirrors the real database shape: argv0 is the compiler, then flags,
// then the source operand.
func entry(file string, args ...string) compdb.Entry {
	argv := append([]string{`C:/Program Files/LLVM/bin/clang-cl.exe`}, args...)
	return compdb.Entry{Directory: "D:/Elpis_UE5.8", File: file, Arguments: append(argv, file)}
}

// noResponses is a reader for fixtures whose flags are already inline.
func noResponses(path string) (string, error) {
	return "", fmt.Errorf("unexpected response file %s", path)
}

func sampleEntries() []compdb.Entry {
	return []compdb.Entry{
		entry(`D:/Elpis_UE5.8/Engine/Source/A.cpp`, `-ID:/Elpis_UE5.8/Engine/Source`, "-DWITH_EDITOR=1"),
		entry(`D:/Elpis_UE5.8/Engine/Source/B.cpp`, `-ID:/Elpis_UE5.8/Engine/Source`),
	}
}

func sampleVersion() BuildVersion {
	return BuildVersion{MajorVersion: 5, MinorVersion: 8, PatchVersion: 1, CompatibleChangelist: 55116800, BranchName: "UE5", BuildID: "custom-5.8"}
}

// The regression the whole identity layer exists for: the same engine at a
// different path must hash identically, where engineindex.Key would not.
func TestStructuralHashIsIndependentOfTheEngineRootPath(t *testing.T) {
	here, err := StructuralHash(`D:/Elpis_UE5.8`, sampleEntries(), sampleVersion(), testRoots(), noResponses)
	if err != nil {
		t.Fatalf("StructuralHash: %v", err)
	}
	moved := []compdb.Entry{
		entry(`C:/UE_5.8/Engine/Source/A.cpp`, `-IC:/UE_5.8/Engine/Source`, "-DWITH_EDITOR=1"),
		entry(`C:/UE_5.8/Engine/Source/B.cpp`, `-IC:/UE_5.8/Engine/Source`),
	}
	movedRoots := testRoots()
	movedRoots.Engine = "C:/UE_5.8"
	there, err := StructuralHash(`C:/UE_5.8`, moved, sampleVersion(), movedRoots, noResponses)
	if err != nil {
		t.Fatalf("StructuralHash: %v", err)
	}
	if here != there {
		t.Fatalf("hash differs across engine roots: %s vs %s", here, there)
	}
}

func TestStructuralHashIsStableUnderEntryOrder(t *testing.T) {
	forward := sampleEntries()
	reversed := []compdb.Entry{forward[1], forward[0]}
	a, err := StructuralHash(`D:/Elpis_UE5.8`, forward, sampleVersion(), testRoots(), noResponses)
	if err != nil {
		t.Fatalf("StructuralHash: %v", err)
	}
	b, err := StructuralHash(`D:/Elpis_UE5.8`, reversed, sampleVersion(), testRoots(), noResponses)
	if err != nil {
		t.Fatalf("StructuralHash: %v", err)
	}
	if a != b {
		t.Fatalf("hash depends on entry order: %s vs %s", a, b)
	}
}

func TestStructuralHashSeparatesDifferentModuleSets(t *testing.T) {
	base, _ := StructuralHash(`D:/Elpis_UE5.8`, sampleEntries(), sampleVersion(), testRoots(), noResponses)
	extra := append(sampleEntries(), entry(`D:/Elpis_UE5.8/Engine/Source/C.cpp`))
	other, err := StructuralHash(`D:/Elpis_UE5.8`, extra, sampleVersion(), testRoots(), noResponses)
	if err != nil {
		t.Fatalf("StructuralHash: %v", err)
	}
	if base == other {
		t.Fatal("adding a source file did not change the hash")
	}
}

func TestStructuralHashSeparatesDifferentCompileArguments(t *testing.T) {
	base, _ := StructuralHash(`D:/Elpis_UE5.8`, sampleEntries(), sampleVersion(), testRoots(), noResponses)
	shipping := []compdb.Entry{
		entry(`D:/Elpis_UE5.8/Engine/Source/A.cpp`, `-ID:/Elpis_UE5.8/Engine/Source`, "-DWITH_EDITOR=0"),
		entry(`D:/Elpis_UE5.8/Engine/Source/B.cpp`, `-ID:/Elpis_UE5.8/Engine/Source`),
	}
	other, err := StructuralHash(`D:/Elpis_UE5.8`, shipping, sampleVersion(), testRoots(), noResponses)
	if err != nil {
		t.Fatalf("StructuralHash: %v", err)
	}
	if base == other {
		t.Fatal("changing a define did not change the hash")
	}
}

func TestStructuralHashSeparatesBuildIDAndLicenseeFlag(t *testing.T) {
	base, _ := StructuralHash(`D:/Elpis_UE5.8`, sampleEntries(), sampleVersion(), testRoots(), noResponses)
	v := sampleVersion()
	v.BuildID = "different"
	other, _ := StructuralHash(`D:/Elpis_UE5.8`, sampleEntries(), v, testRoots(), noResponses)
	if base == other {
		t.Fatal("BuildId is not part of the hash")
	}
	v = sampleVersion()
	v.IsLicenseeVersion = 1
	other, _ = StructuralHash(`D:/Elpis_UE5.8`, sampleEntries(), v, testRoots(), noResponses)
	if base == other {
		t.Fatal("IsLicenseeVersion is not part of the hash")
	}
}

func TestStructuralHashPropagatesAnUnrecognisedAbsolutePath(t *testing.T) {
	bad := []compdb.Entry{entry(`D:/Elpis_UE5.8/Engine/Source/A.cpp`, `-IE:/Other/include`)}
	if _, err := StructuralHash(`D:/Elpis_UE5.8`, bad, sampleVersion(), testRoots(), noResponses); err == nil {
		t.Fatal("StructuralHash accepted an unrecognised absolute path")
	}
}

func TestStructuralHashIsSixteenHexCharacters(t *testing.T) {
	got, err := StructuralHash(`D:/Elpis_UE5.8`, sampleEntries(), sampleVersion(), testRoots(), noResponses)
	if err != nil {
		t.Fatalf("StructuralHash: %v", err)
	}
	if len(got) != 16 {
		t.Fatalf("hash = %q, want 16 hex characters", got)
	}
	for _, c := range got {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("hash %q is not lowercase hex", got)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engineid/... -run StructuralHash -v`
Expected: FAIL to compile, `undefined: StructuralHash`.

- [ ] **Step 3: Write minimal implementation**

```go
package engineid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/softdaddy-o/soft-ue-index/internal/compdb"
	"github.com/softdaddy-o/soft-ue-index/internal/idxrewrite"
)

// StructuralHash fingerprints an engine's module set and build configuration
// without reference to where the engine lives. Every input below is fixed
// deliberately: sort order, separator, case folding, and which argument
// forms carry paths. Two implementations that disagree on any of them
// produce identities that never match, which would be invisible until
// nobody could ever find anybody else's index.
func StructuralHash(engineRoot string, entries []compdb.Entry, v BuildVersion, roots idxrewrite.Roots, read ResponseReader) (string, error) {
	type row struct{ file, dir, compiler, args string }
	rows := make([]row, 0, len(entries))
	for _, e := range entries {
		rel, err := relativeToRoot(engineRoot, e.File)
		if err != nil {
			return "", err
		}
		expanded, compiler, err := ExpandArguments(e, read)
		if err != nil {
			return "", fmt.Errorf("%s: %w", rel, err)
		}
		normalized, err := NormalizeArgs(expanded, roots)
		if err != nil {
			return "", fmt.Errorf("%s: %w", rel, err)
		}
		// Directory decides what a relative argument means, so it belongs in
		// the hash, normalised, because it is an absolute local path.
		dir := normalizeDirectory(e.Directory, roots)
		rows = append(rows, row{
			file:     rel,
			dir:      dir,
			compiler: strings.ToLower(compiler),
			args:     strings.Join(normalized, "\x00"),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].file != rows[j].file {
			return rows[i].file < rows[j].file
		}
		return rows[i].args < rows[j].args
	})
	seen := make(map[row]bool, len(rows))
	h := sha256.New()
	fmt.Fprintf(h, "buildid=%s\nlicensee=%d\n", v.BuildID, v.IsLicenseeVersion)
	for _, r := range rows {
		if seen[r] {
			continue
		}
		seen[r] = true
		for _, field := range []string{r.file, r.dir, r.compiler, r.args} {
			_, _ = io.WriteString(h, field)
			h.Write([]byte{0})
		}
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// ResponseReader reads a response file referenced by an @argument. Injected so
// tests need no real files, and so callers can cache: this project's database
// references 1,234 distinct response files across 26,782 entries, so a naive
// reader opens each one about twenty times.
type ResponseReader func(path string) (string, error)

// CachingResponseReader reads each response file from disk exactly once.
func CachingResponseReader() ResponseReader {
	cache := map[string]string{}
	return func(path string) (string, error) {
		if body, ok := cache[path]; ok {
			return body, nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		cache[path] = string(raw)
		return cache[path], nil
	}
}

// normalizeDirectory renders an entry's working directory in a spelling that
// is the same on every machine. A directory under a known root becomes
// placeholder-relative; a project directory, which by definition is not the
// engine, contributes only its base name. It never returns an absolute path.
func normalizeDirectory(dir string, roots idxrewrite.Roots) string {
	if dir == "" {
		return ""
	}
	if value, err := substitutePath(dir, roots); err == nil {
		return strings.ToLower(value)
	}
	return "project:" + strings.ToLower(filepath.Base(filepath.ToSlash(dir)))
}

// relativeToRoot renders a source path relative to the engine root, in the
// one canonical spelling the hash depends on.
func relativeToRoot(engineRoot, file string) (string, error) {
	root := strings.TrimRight(filepath.ToSlash(engineRoot), "/")
	path := filepath.ToSlash(file)
	if !strings.HasPrefix(strings.ToLower(path), strings.ToLower(root)+"/") {
		return "", fmt.Errorf("entry %q is not under engine root %q", file, engineRoot)
	}
	return strings.ToLower(path[len(root)+1:]), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/engineid/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/engineid/structural.go internal/engineid/structural_test.go
git commit -m "feat(engineid): path-independent structural hash of the engine module set"
```

---

## Task 8: Distribution discriminators

**Files:**
- Create: `internal/engineid/discriminator.go`
- Test: `internal/engineid/discriminator_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `type Discriminator struct { Kind, Value string }`, `type Adapter interface { Kind() string; Detect(engineRoot string) (string, bool, error) }`, `func Adapters() []Adapter`, `func Collect(engineRoot string) ([]Discriminator, error)`.

Adapters return **derived values only**, never raw file contents. Marker files routinely embed internal build paths, and a manifest that carried them would break the no-local-paths invariant.

Four adapters:

| Kind | Source | Derived value |
| --- | --- | --- |
| `installed-build-guid` | `Engine/Build/InstalledBuild.txt` | the GUID, when the file holds one |
| `gitdeps-commit` | `Engine/Build/Commit.gitdeps.xml` | the commit attribute |
| `checksum-manifest` | a `*.manifest` at the engine root whose first line declares a sha256 file list | SHA-256 of that manifest file |
| `provenance` | a small `*.txt` at the engine root containing both a commit-shaped token and a 64-hex checksum | `<commit>+<checksum>`, lowercased |

The provenance adapter matches on shape rather than on any vendor's filename, so it works for any distribution that records a commit and a patch checksum. A commit alone is not an identity: a distribution may apply patches on top of an upstream commit, so both must travel together.

- [ ] **Step 1: Write the failing test**

```go
package engineid

import (
	"os"
	"path/filepath"
	"testing"
)

func engineWith(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

func kinds(ds []Discriminator) map[string]string {
	out := map[string]string{}
	for _, d := range ds {
		out[d.Kind] = d.Value
	}
	return out
}

func TestCollectFindsAnInstalledBuildGUID(t *testing.T) {
	root := engineWith(t, map[string]string{
		"Engine/Build/InstalledBuild.txt": "7CA336D6-AF44-4DAC-A048-88A8150FC037\n",
	})
	got, err := Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if kinds(got)["installed-build-guid"] != "7ca336d6-af44-4dac-a048-88a8150fc037" {
		t.Fatalf("discriminators = %v", kinds(got))
	}
}

// An Epic launcher install has an empty InstalledBuild.txt. That is a
// presence marker, not an identity, and must not become a discriminator.
func TestCollectIgnoresAnEmptyInstalledBuildMarker(t *testing.T) {
	root := engineWith(t, map[string]string{"Engine/Build/InstalledBuild.txt": "\n"})
	got, err := Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if _, ok := kinds(got)["installed-build-guid"]; ok {
		t.Fatalf("empty marker produced a discriminator: %v", kinds(got))
	}
}

func TestCollectFindsAGitdepsCommit(t *testing.T) {
	root := engineWith(t, map[string]string{
		"Engine/Build/Commit.gitdeps.xml": `<?xml version="1.0"?><DependencyManifest BaseUrl="x" Commit="38d3a09de1671f2b3f0b2f1c9d0e5a6b7c8d9e0f"></DependencyManifest>`,
	})
	got, err := Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if kinds(got)["gitdeps-commit"] != "38d3a09de1671f2b3f0b2f1c9d0e5a6b7c8d9e0f" {
		t.Fatalf("discriminators = %v", kinds(got))
	}
}

func TestCollectHashesAChecksumManifest(t *testing.T) {
	root := engineWith(t, map[string]string{
		"Dist.manifest": "# Dist manifest v1  sha256  files=2\nabc  10  a.txt\ndef  20  b.txt\n",
	})
	got, err := Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	value := kinds(got)["checksum-manifest"]
	if len(value) != 64 {
		t.Fatalf("checksum-manifest = %q, want a 64-hex digest", value)
	}
}

func TestChecksumManifestValueIsAHashNotTheContent(t *testing.T) {
	body := "# Dist manifest v1  sha256  files=1\nabc  1  F:/internal/path/a.txt\n"
	root := engineWith(t, map[string]string{"Dist.manifest": body})
	got, err := Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, d := range got {
		if len(d.Value) > 200 {
			t.Fatalf("discriminator %s carries bulk content", d.Kind)
		}
		if filepath.IsAbs(d.Value) {
			t.Fatalf("discriminator %s carries a path: %q", d.Kind, d.Value)
		}
	}
}

func TestCollectPairsAProvenanceCommitWithItsPatchChecksum(t *testing.T) {
	root := engineWith(t, map[string]string{
		"BUILD_MARKER.txt": "Engine: UE 5.8  source 5.8@38d3a09de167\n" +
			"Engine patch:  F:/internal/engine_patches/x.patch\n" +
			"Patch sha256:  C42D9EE3D3282C38547707F40851613FE4A5907E558ABEC170056D28A735293C\n",
	})
	got, err := Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := "38d3a09de167+c42d9ee3d3282c38547707f40851613fe4a5907e558abec170056d28a735293c"
	if kinds(got)["provenance"] != want {
		t.Fatalf("provenance = %q, want %q", kinds(got)["provenance"], want)
	}
}

// The marker file above contains an internal path. It must not survive into
// the derived value.
func TestProvenanceValueOmitsInternalPaths(t *testing.T) {
	root := engineWith(t, map[string]string{
		"BUILD_MARKER.txt": "source 5.8@38d3a09de167\npatch F:/internal/engine_patches/x.patch\n" +
			"sha256 c42d9ee3d3282c38547707f40851613fe4a5907e558abec170056d28a735293c\n",
	})
	got, err := Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, d := range got {
		if filepath.IsAbs(d.Value) || len(d.Value) > 200 {
			t.Fatalf("discriminator %s leaks content: %q", d.Kind, d.Value)
		}
	}
}

func TestCollectReturnsNothingForAPlainEngine(t *testing.T) {
	got, err := Collect(engineWith(t, map[string]string{"Engine/Build/Build.version": "{}"}))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("discriminators = %v, want none", kinds(got))
	}
}

func TestCollectIsDeterministicallyOrdered(t *testing.T) {
	root := engineWith(t, map[string]string{
		"Engine/Build/InstalledBuild.txt": "7CA336D6-AF44-4DAC-A048-88A8150FC037",
		"Dist.manifest":                   "# Dist manifest v1  sha256  files=1\nabc  1  a.txt\n",
	})
	first, _ := Collect(root)
	second, _ := Collect(root)
	if len(first) != len(second) {
		t.Fatalf("length differs between runs")
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("order differs at %d: %v vs %v", i, first[i], second[i])
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engineid/... -run 'Collect|Provenance|Checksum' -v`
Expected: FAIL to compile, `undefined: Collect`.

- [ ] **Step 3: Write minimal implementation**

```go
package engineid

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Discriminator is a derived marker that narrows an engine identity beyond
// its primary key. Value is always a derived token -- a hash, a GUID, a
// commit -- never raw file content, because marker files routinely embed
// internal build paths that must not reach a published manifest.
type Discriminator struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Adapter detects one distribution's marker. Detect reports ok=false when
// this distribution's marker is absent, which is the common case.
type Adapter interface {
	Kind() string
	Detect(engineRoot string) (value string, ok bool, err error)
}

// markerFileLimit caps how much of a marker file is read. Markers are small;
// anything larger is not one.
const markerFileLimit = 64 << 10

var (
	guidPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	commitAttr      = regexp.MustCompile(`(?i)Commit\s*=\s*"([0-9a-fA-F]{7,40})"`)
	commitInText    = regexp.MustCompile(`@([0-9a-fA-F]{7,40})\b`)
	sha256InText    = regexp.MustCompile(`\b([0-9a-fA-F]{64})\b`)
	manifestHeading = regexp.MustCompile(`(?i)^#.*sha256`)
)

// Adapters returns every detector, in a fixed order so that Collect's output
// is deterministic.
func Adapters() []Adapter {
	return []Adapter{
		checksumManifestAdapter{},
		provenanceAdapter{},
		installedBuildAdapter{},
		gitdepsAdapter{},
	}
}

// Collect runs every adapter and returns the markers that were found,
// sorted by kind.
func Collect(engineRoot string) ([]Discriminator, error) {
	var out []Discriminator
	for _, a := range Adapters() {
		value, ok, err := a.Detect(engineRoot)
		if err != nil {
			return nil, err
		}
		if ok && value != "" {
			out = append(out, Discriminator{Kind: a.Kind(), Value: value})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out, nil
}

func readMarker(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > markerFileLimit {
		return "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

type installedBuildAdapter struct{}

func (installedBuildAdapter) Kind() string { return "installed-build-guid" }

// Detect reads the installed-build marker. An Epic launcher install leaves
// this file empty, which is a presence flag rather than an identity, so only
// a real GUID counts.
func (installedBuildAdapter) Detect(engineRoot string) (string, bool, error) {
	body, ok := readMarker(filepath.Join(engineRoot, "Engine", "Build", "InstalledBuild.txt"))
	if !ok {
		return "", false, nil
	}
	value := strings.TrimSpace(body)
	if !guidPattern.MatchString(value) {
		return "", false, nil
	}
	return strings.ToLower(value), true, nil
}

type gitdepsAdapter struct{}

func (gitdepsAdapter) Kind() string { return "gitdeps-commit" }

func (gitdepsAdapter) Detect(engineRoot string) (string, bool, error) {
	body, ok := readMarker(filepath.Join(engineRoot, "Engine", "Build", "Commit.gitdeps.xml"))
	if !ok {
		return "", false, nil
	}
	m := commitAttr.FindStringSubmatch(body)
	if m == nil {
		return "", false, nil
	}
	return strings.ToLower(m[1]), true, nil
}

type checksumManifestAdapter struct{}

func (checksumManifestAdapter) Kind() string { return "checksum-manifest" }

// Detect hashes a per-file checksum manifest shipped at the engine root.
// Hashing the manifest is the cheapest exact identity available: one digest
// stands in for every file the distribution listed.
func (checksumManifestAdapter) Detect(engineRoot string) (string, bool, error) {
	matches, err := filepath.Glob(filepath.Join(engineRoot, "*.manifest"))
	if err != nil {
		return "", false, err
	}
	sort.Strings(matches)
	for _, path := range matches {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		header := make([]byte, 256)
		n, _ := f.Read(header)
		firstLine := strings.SplitN(string(header[:n]), "\n", 2)[0]
		if !manifestHeading.MatchString(strings.TrimSpace(firstLine)) {
			f.Close()
			continue
		}
		if _, err := f.Seek(0, 0); err != nil {
			f.Close()
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return "", false, err
		}
		f.Close()
		return hex.EncodeToString(h.Sum(nil)), true, nil
	}
	return "", false, nil
}

type provenanceAdapter struct{}

func (provenanceAdapter) Kind() string { return "provenance" }

// Detect pairs an upstream commit with the checksum of whatever patch the
// distribution applied on top of it. A commit alone is not an identity: two
// builds can share a commit and differ by a patch. Matching is by shape, not
// by any vendor's filename, and only the two derived tokens are returned --
// these marker files commonly also record internal build paths.
func (provenanceAdapter) Detect(engineRoot string) (string, bool, error) {
	matches, err := filepath.Glob(filepath.Join(engineRoot, "*.txt"))
	if err != nil {
		return "", false, err
	}
	sort.Strings(matches)
	for _, path := range matches {
		body, ok := readMarker(path)
		if !ok {
			continue
		}
		commit := commitInText.FindStringSubmatch(body)
		digest := sha256InText.FindStringSubmatch(body)
		if commit == nil || digest == nil {
			continue
		}
		return strings.ToLower(commit[1]) + "+" + strings.ToLower(digest[1]), true, nil
	}
	return "", false, nil
}
```

The import block for this file is:

```go
import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/engineid/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/engineid/discriminator.go internal/engineid/discriminator_test.go
git commit -m "feat(engineid): pluggable distribution discriminators, derived values only"
```

---

## Task 9: Identity assembly and match policy

**Files:**
- Create: `internal/engineid/identity.go`
- Test: `internal/engineid/identity_test.go`

**Interfaces:**
- Consumes: Tasks 5, 7, 8.
- Produces: `type Identity struct { Primary string; Version BuildVersion; Structural string; Discriminators []Discriminator }`, `func Compute(engineRoot string, entries []compdb.Entry, roots idxrewrite.Roots, read ResponseReader) (Identity, error)`, `type Strength int` with `NoMatch`, `PrimaryOnly`, `Confirmed`, `func Match(local, remote Identity) Strength`, `func (s Strength) String() string`.

The match policy from the spec:

- Candidates must agree on the primary key, or there is no match at all.
- If both sides carry the same discriminator kind and the values differ, the candidate is **rejected** outright — a shared marker that disagrees is stronger evidence against than the primary key is for.
- If both sides carry a shared kind and every shared kind agrees, the match is `Confirmed`.
- If no kind is shared, the match is `PrimaryOnly`, which the CLI surfaces as a warning requiring confirmation.

- [ ] **Step 1: Write the failing test**

```go
package engineid

import (
	"strings"
	"testing"
)

func idWith(primary string, ds ...Discriminator) Identity {
	return Identity{Primary: primary, Discriminators: ds}
}

func TestComputeBuildsAReadablePrimaryKey(t *testing.T) {
	root := writeBuildVersion(t, `{
		"MajorVersion": 5, "MinorVersion": 8, "PatchVersion": 1,
		"CompatibleChangelist": 55116800, "BranchName": "UE5", "BuildId": "custom-5.8"
	}`)
	entries := []compdb.Entry{entry(rootJoin(root, "Engine/Source/A.cpp"), "-DWITH_EDITOR=1")}
	roots := testRoots()
	roots.Engine = toSlash(root)
	got, err := Compute(root, entries, roots, noResponses)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if !strings.HasPrefix(got.Primary, "5.8.1-cl55116800-UE5-") {
		t.Fatalf("primary = %q, want the readable label as its prefix", got.Primary)
	}
	if got.Structural == "" || !strings.HasSuffix(got.Primary, got.Structural) {
		t.Fatalf("primary %q does not end with structural hash %q", got.Primary, got.Structural)
	}
}

func TestMatchRejectsDifferentPrimaryKeys(t *testing.T) {
	if got := Match(idWith("a"), idWith("b")); got != NoMatch {
		t.Fatalf("Match = %v, want NoMatch", got)
	}
}

func TestMatchIsPrimaryOnlyWithNoSharedDiscriminator(t *testing.T) {
	local := idWith("same", Discriminator{"provenance", "x"})
	remote := idWith("same", Discriminator{"gitdeps-commit", "y"})
	if got := Match(local, remote); got != PrimaryOnly {
		t.Fatalf("Match = %v, want PrimaryOnly", got)
	}
}

func TestMatchIsConfirmedOnlyWhenBothSidesCarryTheSameKinds(t *testing.T) {
	local := idWith("same", Discriminator{"provenance", "x"}, Discriminator{"gitdeps-commit", "c"})
	remote := idWith("same", Discriminator{"provenance", "x"}, Discriminator{"gitdeps-commit", "c"})
	if got := Match(local, remote); got != Confirmed {
		t.Fatalf("Match = %v, want Confirmed", got)
	}
}

// A manifest that omits the marker which would disagree must not be able to
// buy a confirmation by staying silent.
func TestMatchStaysPrimaryOnlyWhenTheRemoteOmitsAKind(t *testing.T) {
	local := idWith("same", Discriminator{"provenance", "x"}, Discriminator{"gitdeps-commit", "c"})
	remote := idWith("same", Discriminator{"provenance", "x"})
	if got := Match(local, remote); got != PrimaryOnly {
		t.Fatalf("Match = %v, want PrimaryOnly", got)
	}
	if kinds := UnconfirmedKinds(local, remote); len(kinds) != 1 || kinds[0] != "gitdeps-commit" {
		t.Fatalf("UnconfirmedKinds = %v, want [gitdeps-commit]", kinds)
	}
}

// A shared marker that disagrees is stronger evidence against a match than
// the primary key is for it. Installing anyway would put an index built from
// different engine content onto this machine.
func TestMatchRejectsWhenASharedDiscriminatorDisagrees(t *testing.T) {
	local := idWith("same", Discriminator{"provenance", "x"})
	remote := idWith("same", Discriminator{"provenance", "DIFFERENT"})
	if got := Match(local, remote); got != NoMatch {
		t.Fatalf("Match = %v, want NoMatch", got)
	}
}

func TestMatchRejectsWhenAnyOfSeveralSharedDiscriminatorsDisagrees(t *testing.T) {
	local := idWith("same", Discriminator{"provenance", "x"}, Discriminator{"gitdeps-commit", "c"})
	remote := idWith("same", Discriminator{"provenance", "x"}, Discriminator{"gitdeps-commit", "OTHER"})
	if got := Match(local, remote); got != NoMatch {
		t.Fatalf("Match = %v, want NoMatch", got)
	}
}

func TestStrengthStringsAreHumanReadable(t *testing.T) {
	for s, want := range map[Strength]string{
		NoMatch:     "no match",
		PrimaryOnly: "primary key only",
		Confirmed:   "confirmed",
	} {
		if got := s.String(); got != want {
			t.Fatalf("Strength(%d) = %q, want %q", s, got, want)
		}
	}
}
```

Add these two helpers to the same test file so the test above compiles, and
add `path/filepath` and `github.com/softdaddy-o/soft-ue-index/internal/compdb`
to that file's single import block:

```go
func rootJoin(root, rel string) string {
	return filepath.ToSlash(filepath.Join(root, filepath.FromSlash(rel)))
}

func toSlash(p string) string { return filepath.ToSlash(p) }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engineid/... -run 'Compute|Match|Strength' -v`
Expected: FAIL to compile, `undefined: Compute`.

- [ ] **Step 3: Write minimal implementation**

```go
package engineid

import (
	"fmt"

	"github.com/softdaddy-o/soft-ue-index/internal/compdb"
	"github.com/softdaddy-o/soft-ue-index/internal/idxrewrite"
)

// Identity names an engine install in a way that does not depend on where it
// is installed, so an index built elsewhere can be matched to it.
type Identity struct {
	Primary        string          `json:"primary"`
	Version        BuildVersion    `json:"version"`
	Structural     string          `json:"structural"`
	Discriminators []Discriminator `json:"discriminators,omitempty"`
}

// Compute derives the identity of the engine at engineRoot, given the
// engine-scoped entries of a compilation database generated against it.
func Compute(engineRoot string, entries []compdb.Entry, roots idxrewrite.Roots, read ResponseReader) (Identity, error) {
	version, err := ReadBuildVersion(engineRoot)
	if err != nil {
		return Identity{}, err
	}
	structural, err := StructuralHash(engineRoot, entries, version, roots, read)
	if err != nil {
		return Identity{}, err
	}
	discriminators, err := Collect(engineRoot)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		Primary:        fmt.Sprintf("%s-%s", version.Label(), structural),
		Version:        version,
		Structural:     structural,
		Discriminators: discriminators,
	}, nil
}

// Strength grades how confidently a remote index matches a local engine.
type Strength int

// Match strengths, ascending.
const (
	NoMatch Strength = iota
	PrimaryOnly
	Confirmed
)

func (s Strength) String() string {
	switch s {
	case Confirmed:
		return "confirmed"
	case PrimaryOnly:
		return "primary key only"
	default:
		return "no match"
	}
}

// Match applies the spec's policy.
//
// The primary key must agree. Any discriminator kind present on both sides
// must agree, or the candidate is rejected outright: a shared marker that
// disagrees is stronger evidence against a match than the primary key is for
// it.
//
// Confirmed requires the two sides to carry the SAME SET of kinds. Agreeing
// on the overlap is not enough, because a manifest that simply omits the one
// marker that would disagree could otherwise buy a confirmation by staying
// silent. Anything short of full coverage stays PrimaryOnly, which callers
// must surface as a warning rather than install silently.
func Match(local, remote Identity) Strength {
	if local.Primary == "" || local.Primary != remote.Primary {
		return NoMatch
	}
	remoteByKind := make(map[string]string, len(remote.Discriminators))
	for _, d := range remote.Discriminators {
		remoteByKind[d.Kind] = d.Value
	}
	localByKind := make(map[string]string, len(local.Discriminators))
	for _, d := range local.Discriminators {
		localByKind[d.Kind] = d.Value
	}
	shared := 0
	for kind, value := range localByKind {
		remoteValue, ok := remoteByKind[kind]
		if !ok {
			continue
		}
		if remoteValue != value {
			return NoMatch
		}
		shared++
	}
	if shared == 0 {
		return PrimaryOnly
	}
	if shared != len(localByKind) || shared != len(remoteByKind) {
		return PrimaryOnly
	}
	return Confirmed
}

// UnconfirmedKinds names the discriminator kinds that only one side carries,
// so a PrimaryOnly warning can say what specifically could not be checked
// instead of just asserting that something could not.
func UnconfirmedKinds(local, remote Identity) []string {
	present := map[string]int{}
	for _, d := range local.Discriminators {
		present[d.Kind] |= 1
	}
	for _, d := range remote.Discriminators {
		present[d.Kind] |= 2
	}
	var out []string
	for kind, seen := range present {
		if seen != 3 {
			out = append(out, kind)
		}
	}
	sort.Strings(out)
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/engineid/... -v`
Expected: PASS, all engineid tests.

- [ ] **Step 5: Commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/engineid/identity.go internal/engineid/identity_test.go
git commit -m "feat(engineid): assemble engine identity and apply the match policy"
```

---

## Task 10: Manifest, compatibility gates, and packaging

**Files:**
- Create: `internal/indexshare/manifest.go`
- Create: `internal/indexshare/pack.go`
- Test: `internal/indexshare/manifest_test.go`
- Test: `internal/indexshare/pack_test.go`

**Interfaces:**
- Consumes: `engineid.Identity`, `engineid.BuildVersion`, `engineid.Discriminator`, `idxrewrite.IndexFormatVersion`.
- Produces: `const SchemaVersion = 1`, `type Manifest struct{...}`, `func (m Manifest) Validate() error`, `func (m Manifest) CheckCompatible(localIndexFormat, actualFormat uint32) error`, `func AssetKey(engineID string) string`, `func ArtifactName(engineID string) string`, `func ManifestName(engineID string) string`, `func Pack(idx []byte, m Manifest) ([]byte, Manifest, error)`, `func Unpack(artifact []byte, m Manifest) ([]byte, error)`, `var ErrIncompatible error`, `var ErrCorrupt error`.

The compatibility gate is deliberately two-part. Deriving the index format version the local clangd expects is not practical at install time, so the **clangd major version** is the operative gate, and the `meta` value is a corruption and mislabelling check against what the manifest claims.

- [ ] **Step 1: Write the failing test**

```go
package indexshare

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/softdaddy-o/soft-ue-index/internal/engineid"
	"github.com/softdaddy-o/soft-ue-index/internal/idxrewrite"
)

func sampleManifest() Manifest {
	digest := strings.Repeat("ab", 32)
	return Manifest{
		SchemaVersion:  SchemaVersion,
		EngineID:       "5.8.1-cl55116800-UE5-0123456789abcdef",
		Structural:     "0123456789abcdef",
		Version:        engineid.BuildVersion{MajorVersion: 5, MinorVersion: 8, PatchVersion: 1},
		ClangdVersion:  "20.1.8",
		IndexFormat:    20,
		Placeholders: []string{
			idxrewrite.EnginePlaceholder, idxrewrite.SDKPlaceholder,
			idxrewrite.MSVCPlaceholder, idxrewrite.ClangPlaceholder,
		},
		ArtifactSHA256: digest,
		IndexSHA256:    digest,
		ArtifactBytes:  348241956,
		IndexBytes:     565237356,
		EngineEntries:  24996,
		URICount:       66484,
		PublishedAt:    time.Unix(0, 0).UTC(),
	}
}

func TestValidateRejectsAnUnknownSchemaVersion(t *testing.T) {
	m := sampleManifest()
	m.SchemaVersion = 99
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted an unknown schema version")
	}
}

func TestValidateRejectsAMissingEngineID(t *testing.T) {
	m := sampleManifest()
	m.EngineID = ""
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted an empty engine id")
	}
}

// The manifest ships publicly. A local path in any field breaks the
// no-local-paths invariant the whole publish path is built around.
func TestValidateRejectsALocalPathAnywhereInTheManifest(t *testing.T) {
	m := sampleManifest()
	m.Discriminators = []engineid.Discriminator{{Kind: "provenance", Value: `F:/internal/patches/x.patch`}}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("Validate err = %v, want a complaint about a path", err)
	}
}

func TestCheckCompatibleAcceptsAFormatTheLocalClangdReads(t *testing.T) {
	if err := sampleManifest().CheckCompatible(20, 20); err != nil {
		t.Fatalf("CheckCompatible: %v", err)
	}
}

// clangd's format constant moves independently of its release version, so the
// gate must compare formats, not versions.
func TestCheckCompatibleRejectsAFormatTheLocalClangdDoesNotRead(t *testing.T) {
	err := sampleManifest().CheckCompatible(21, 20)
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("err = %v, want ErrIncompatible", err)
	}
}

func TestCheckCompatibleRejectsAnUnknownLocalFormat(t *testing.T) {
	if err := sampleManifest().CheckCompatible(0, 20); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("err = %v, want ErrIncompatible", err)
	}
}

// The artifact claiming one format while carrying another means it was
// mislabelled or corrupted in transit.
func TestCheckCompatibleRejectsAFormatDisagreeingWithTheManifest(t *testing.T) {
	err := sampleManifest().CheckCompatible(20, 19)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// A filename built from engine metadata is a path-traversal vector: BranchName
// is arbitrary text read off disk.
func TestAssetNamesAreDigestsAndNeverCarryEngineMetadata(t *testing.T) {
	hostile := `5.8.1-cl1-../../../../etc/UE5-0123456789abcdef`
	for _, name := range []string{ArtifactName(hostile), ManifestName(hostile)} {
		if strings.ContainsAny(name, `/\:`) || strings.Contains(name, "..") {
			t.Fatalf("asset name %q is not filename-safe", name)
		}
	}
	if ArtifactName("a") == ArtifactName("b") {
		t.Fatal("distinct engine ids collided on one asset name")
	}
	if ArtifactName("a") != ArtifactName("a") {
		t.Fatal("asset name is not stable")
	}
}

func TestValidateRejectsAMissingDigest(t *testing.T) {
	m := sampleManifest()
	m.ArtifactSHA256 = ""
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted a manifest with no artifact digest; an optional integrity check is a bypass")
	}
}

func TestValidateRejectsANonCanonicalDigest(t *testing.T) {
	m := sampleManifest()
	m.ArtifactSHA256 = strings.ToUpper(m.ArtifactSHA256)
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted an uppercase digest")
	}
}

func TestValidateRejectsDuplicateDiscriminatorKinds(t *testing.T) {
	m := sampleManifest()
	m.Discriminators = []engineid.Discriminator{{Kind: "provenance", Value: "a"}, {Kind: "provenance", Value: "b"}}
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted a duplicated discriminator kind")
	}
}

func TestValidateRejectsAnEngineIDInconsistentWithItsStructuralHash(t *testing.T) {
	m := sampleManifest()
	m.Structural = "ffffffffffffffff"
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted an engineId that does not end with its structural hash")
	}
}

func TestValidateRejectsAnUnexpectedPlaceholderSet(t *testing.T) {
	m := sampleManifest()
	m.Placeholders = []string{"SOMETHING_ELSE"}
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted an unexpected placeholder set")
	}
}
```

And `pack_test.go`:

```go
package indexshare

import (
	"bytes"
	"errors"
	"testing"
)

func TestPackFillsBothDigestsAndUnpackRestoresTheIndex(t *testing.T) {
	idx := []byte("pretend this is a clangd index")
	artifact, m, err := Pack(idx, sampleManifest())
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if m.ArtifactSHA256 == "" || m.IndexSHA256 == "" {
		t.Fatalf("Pack left a digest empty: %+v", m)
	}
	got, err := Unpack(artifact, m)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if !bytes.Equal(got, idx) {
		t.Fatalf("Unpack returned %q, want %q", got, idx)
	}
}

func TestUnpackRejectsATamperedArtifact(t *testing.T) {
	artifact, m, err := Pack([]byte("index"), sampleManifest())
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	artifact[len(artifact)-1] ^= 0xff
	if _, err := Unpack(artifact, m); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestUnpackRejectsAnIndexDigestMismatch(t *testing.T) {
	artifact, m, err := Pack([]byte("index"), sampleManifest())
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	m.IndexSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := Unpack(artifact, m); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/indexshare/... -v`
Expected: FAIL to compile, `undefined: Manifest`.

- [ ] **Step 3: Write manifest.go**

```go
// Package indexshare publishes and fetches prebuilt clangd engine indexes.
package indexshare

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/softdaddy-o/soft-ue-index/internal/engineid"
	"github.com/softdaddy-o/soft-ue-index/internal/idxrewrite"
)

// SchemaVersion is the manifest format this build writes and accepts.
const SchemaVersion = 1

// ErrIncompatible reports an artifact this machine's clangd cannot use.
var ErrIncompatible = errors.New("incompatible engine index artifact")

// ErrCorrupt reports an artifact whose bytes disagree with its manifest.
var ErrCorrupt = errors.New("corrupt engine index artifact")

// Manifest describes one published index. It carries no local paths: every
// value is derived, so publishing cannot leak a username or a build layout.
type Manifest struct {
	SchemaVersion  int                      `json:"schemaVersion"`
	EngineID       string                   `json:"engineId"`
	Version        engineid.BuildVersion    `json:"engineVersion"`
	Structural     string                   `json:"structural"`
	Discriminators []engineid.Discriminator `json:"discriminators,omitempty"`
	ClangdVersion  string                   `json:"clangdVersion"`
	IndexFormat    uint32                   `json:"indexFormatVersion"`
	Placeholders   []string                 `json:"placeholders"`
	ArtifactSHA256 string                   `json:"artifactSha256"`
	IndexSHA256    string                   `json:"indexSha256"`
	ArtifactBytes  int64                    `json:"artifactBytes"`
	IndexBytes     int64                    `json:"indexBytes"`
	EngineEntries  int                      `json:"engineEntries"`
	URICount       int                      `json:"uriCount"`
	PublishedAt    time.Time                `json:"publishedAt"`
}

// Identity reconstructs the engine identity this manifest describes, so that
// engineid.Match can grade it against the local engine.
func (m Manifest) Identity() engineid.Identity {
	return engineid.Identity{
		Primary:        m.EngineID,
		Version:        m.Version,
		Structural:     m.Structural,
		Discriminators: m.Discriminators,
	}
}

// Validate checks the manifest is well formed, path-free, and carries every
// field the install path relies on.
//
// Every check here runs against a document fetched over the network. An
// optional field is an optional check, and an optional integrity check is a
// bypass, so nothing that guards installation is allowed to be empty.
func (m Manifest) Validate() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: manifest schema %d, this build understands %d", ErrIncompatible, m.SchemaVersion, SchemaVersion)
	}
	for field, value := range map[string]string{
		"engineId":      m.EngineID,
		"structural":    m.Structural,
		"clangdVersion": m.ClangdVersion,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: manifest has no %s", ErrIncompatible, field)
		}
	}
	for field, value := range map[string]string{
		"artifactSha256": m.ArtifactSHA256,
		"indexSha256":    m.IndexSHA256,
	} {
		if !isSHA256Hex(value) {
			return fmt.Errorf("%w: %s is not a 64-character lowercase hex digest", ErrIncompatible, field)
		}
	}
	if m.ArtifactBytes <= 0 || m.IndexBytes <= 0 {
		return fmt.Errorf("%w: manifest must declare positive artifactBytes and indexBytes", ErrIncompatible)
	}
	if m.IndexFormat == 0 {
		return fmt.Errorf("%w: manifest declares no index format version", ErrIncompatible)
	}
	if m.EngineEntries < 0 || m.URICount < 0 {
		return fmt.Errorf("%w: manifest declares a negative count", ErrIncompatible)
	}
	if !strings.HasSuffix(m.EngineID, m.Structural) {
		return fmt.Errorf("%w: engineId %q does not end with its structural hash %q", ErrIncompatible, m.EngineID, m.Structural)
	}
	if err := validatePlaceholders(m.Placeholders); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, d := range m.Discriminators {
		if seen[d.Kind] {
			return fmt.Errorf("%w: duplicate discriminator kind %q", ErrIncompatible, d.Kind)
		}
		seen[d.Kind] = true
		if strings.TrimSpace(d.Value) == "" {
			return fmt.Errorf("%w: discriminator %q has no value", ErrIncompatible, d.Kind)
		}
		if idxrewrite.HasAbsolutePath(d.Value) {
			return fmt.Errorf("%w: discriminator %q carries a local path", ErrIncompatible, d.Kind)
		}
	}
	if idxrewrite.HasAbsolutePath(m.EngineID) || idxrewrite.HasAbsolutePath(m.Version.BuildID) {
		return fmt.Errorf("%w: manifest carries a local path", ErrIncompatible)
	}
	return nil
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func validatePlaceholders(got []string) error {
	want := []string{
		idxrewrite.EnginePlaceholder, idxrewrite.SDKPlaceholder,
		idxrewrite.MSVCPlaceholder, idxrewrite.ClangPlaceholder,
	}
	if len(got) != len(want) {
		return fmt.Errorf("%w: manifest declares %d placeholders, expected %d", ErrIncompatible, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("%w: placeholder %d is %q, expected %q", ErrIncompatible, i, got[i], want[i])
		}
	}
	return nil
}

// CheckCompatible gates installation.
//
// It compares the artifact against the format the LOCAL clangd actually
// accepts, measured by ProbeIndexFormat, not against a clangd version number.
// clangd's index format constant is independent of its release version and
// can change without a major bump, so a version comparison is a proxy that
// can be wrong in both directions. The second check, artifact against its own
// manifest, catches a mislabelled or damaged download.
func (m Manifest) CheckCompatible(localIndexFormat, actualFormat uint32) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if actualFormat != m.IndexFormat {
		return fmt.Errorf("%w: index declares format %d but its manifest claims %d", ErrCorrupt, actualFormat, m.IndexFormat)
	}
	if localIndexFormat == 0 {
		return fmt.Errorf("%w: could not determine the index format this machine's clangd accepts", ErrIncompatible)
	}
	if actualFormat != localIndexFormat {
		return fmt.Errorf("%w: artifact is index format %d, this machine's clangd reads format %d (published from clangd %s) -- clangd refuses any other version outright",
			ErrIncompatible, actualFormat, localIndexFormat, m.ClangdVersion)
	}
	return nil
}

// AssetKey is the filename-safe key for an identity.
//
// The readable engine label is built from Build.version, whose BranchName is
// arbitrary text read off disk. Using it in a filename would let a slash,
// colon, or ".." escape the staging directory or produce an asset name GitHub
// rejects. So the filename is a digest and the readable label lives only
// inside the manifest, where it cannot become a path component.
func AssetKey(engineID string) string {
	sum := sha256.Sum256([]byte(engineID))
	return hex.EncodeToString(sum[:])[:32]
}

// ArtifactName is the release asset name holding the compressed index.
func ArtifactName(engineID string) string { return "engine-index-" + AssetKey(engineID) + ".idx.gz" }

// ManifestName is the release asset name holding the manifest.
func ManifestName(engineID string) string { return "engine-index-" + AssetKey(engineID) + ".json" }
```

- [ ] **Step 4: Write pack.go**

```go
package indexshare

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// Pack compresses a normalised index and records both digests. The index
// digest is taken before compression so that a decompressed artifact can be
// checked against what was published, not merely against its own envelope.
func Pack(idx []byte, m Manifest) ([]byte, Manifest, error) {
	m.IndexSHA256 = digest(idx)
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, m, err
	}
	if _, err := w.Write(idx); err != nil {
		return nil, m, fmt.Errorf("compress index: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, m, fmt.Errorf("finish compressing index: %w", err)
	}
	artifact := buf.Bytes()
	m.ArtifactSHA256 = digest(artifact)
	m.IndexBytes = int64(len(idx))
	m.ArtifactBytes = int64(len(artifact))
	return artifact, m, nil
}

// Unpack verifies the artifact digest before decompressing, decompresses
// under a declared bound, then verifies the decompressed index.
//
// Both digests are required, never "verify if present". Validate has already
// rejected an absent or malformed digest by the time this runs, so there is
// no path here that skips a check because a field was empty.
func Unpack(artifact []byte, m Manifest) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if int64(len(artifact)) != m.ArtifactBytes {
		return nil, fmt.Errorf("%w: artifact is %d bytes, manifest declares %d", ErrCorrupt, len(artifact), m.ArtifactBytes)
	}
	if got := digest(artifact); got != m.ArtifactSHA256 {
		return nil, fmt.Errorf("%w: artifact digest %s does not match the manifest's %s", ErrCorrupt, got, m.ArtifactSHA256)
	}
	r, err := gzip.NewReader(bytes.NewReader(artifact))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	defer r.Close()
	// Read exactly the declared length, then require EOF. io.ReadAll here
	// would let a validly-digested gzip bomb decide how much memory to take.
	idx := make([]byte, m.IndexBytes)
	if _, err := io.ReadFull(r, idx); err != nil {
		return nil, fmt.Errorf("%w: decompress: %v", ErrCorrupt, err)
	}
	var extra [1]byte
	if n, err := r.Read(extra[:]); n != 0 || err != io.EOF {
		return nil, fmt.Errorf("%w: artifact expands past its declared %d bytes", ErrCorrupt, m.IndexBytes)
	}
	if got := digest(idx); got != m.IndexSHA256 {
		return nil, fmt.Errorf("%w: index digest %s does not match the manifest's %s", ErrCorrupt, got, m.IndexSHA256)
	}
	return idx, nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/indexshare/... -v`
Expected: PASS.

- [ ] **Step 6: Measure the local index format**

`CheckCompatible` needs the format this machine's clangd actually reads.
clangd's format constant moves independently of its release version, so it is
measured, not inferred. Create `internal/engineindex/probe.go`:

```go
package engineindex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/softdaddy-o/soft-ue-index/internal/idxrewrite"
)

// ProbeIndexFormat reports the index format version the local clangd-indexer
// emits, by building a one-file index and reading its meta chunk.
//
// This exists because clangd's format constant is not tied to its release
// version: comparing clangd versions is a proxy that can be wrong in both
// directions, and clangd refuses any format but its own outright. The probe
// costs one trivial compile, so measuring beats guessing.
func ProbeIndexFormat(ctx context.Context, runner Runner, indexerPath string) (uint32, error) {
	dir, err := os.MkdirTemp("", "soft-ue-index-probe-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "probe.cpp")
	if err := os.WriteFile(src, []byte("int softUeIndexProbe() { return 0; }\n"), 0o600); err != nil {
		return 0, err
	}
	db := []map[string]any{{
		"directory": filepath.ToSlash(dir),
		"file":      filepath.ToSlash(src),
		"arguments": []string{"clang++", "-std=c++17", "-c", filepath.ToSlash(src)},
	}}
	body, err := json.Marshal(db)
	if err != nil {
		return 0, err
	}
	dbPath := filepath.Join(dir, "compile_commands.json")
	if err := os.WriteFile(dbPath, body, 0o600); err != nil {
		return 0, err
	}
	idxPath := filepath.Join(dir, "probe.idx")
	logPath := filepath.Join(dir, "probe.log")
	if err := BuildIndex(ctx, runner, indexerPath, dbPath, idxPath, logPath); err != nil {
		return 0, fmt.Errorf("probe the local index format: %w", err)
	}
	raw, err := os.ReadFile(idxPath)
	if err != nil {
		return 0, err
	}
	return idxrewrite.IndexFormatVersion(raw)
}
```

Test it in `internal/engineindex/probe_test.go` with a fake `Runner` that writes
a fixture index to the staging path, asserting the probe returns that fixture's
meta value and that it surfaces a runner failure rather than returning zero
silently. Add a second test guarded by `SOFT_UE_INDEX_TEST_INDEXER`, skipped
when unset, that runs the probe against a real `clangd-indexer` and asserts a
nonzero result -- the fake proves the plumbing, only the real binary proves the
number.

The result is cached per resolved clangd-indexer path for the process lifetime;
recomputing it per command would add a compile to every `engine-index list`.

- [ ] **Step 7: Commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/indexshare/manifest.go internal/indexshare/pack.go internal/indexshare/manifest_test.go internal/indexshare/pack_test.go internal/engineindex/probe.go internal/engineindex/probe_test.go
git commit -m "feat(indexshare): manifest schema, measured format gate, and packaging"
```

---

## Task 11: GitHub release transport

**Files:**
- Create: `internal/indexshare/release.go`
- Test: `internal/indexshare/release_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `type Asset struct { Name, URL string; Size int64 }`, `type Release struct { Tag string; Assets []Asset }`, `type Uploader func(ctx context.Context, repo, tag string, files []string, force bool) error`, `type Client struct { Repo string; BaseURL string; HTTP *http.Client; Upload Uploader }`, `func NewClient(repo string) *Client`, `func (c *Client) Releases(ctx context.Context) ([]Release, error)`, `func (c *Client) Fetch(ctx context.Context, url string) ([]byte, error)`, `func (c *Client) Publish(ctx context.Context, tag string, files []string) error`, `func GHUploader(ctx context.Context, repo, tag string, files []string) error`.

Download uses `net/http` because a public release asset needs no authentication, which keeps the module dependency-free. Upload shells out to `gh`, which already owns token storage and refresh.

`BaseURL` exists so tests can point the client at an `httptest` server. `Upload` is a field so tests can substitute a recorder for `gh`.

- [ ] **Step 1: Write the failing test**

```go
package indexshare

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := NewClient("owner/repo")
	c.BaseURL = server.URL
	return c
}

func TestReleasesParsesTagsAndAssets(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/repos/owner/repo/releases") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"tag_name":"engine-index","assets":[
			{"name":"engine-index-abc.json","browser_download_url":"http://x/a.json","size":12},
			{"name":"engine-index-abc.idx.gz","browser_download_url":"http://x/a.gz","size":345}
		]}]`))
	})
	got, err := c.Releases(context.Background())
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if len(got) != 1 || got[0].Tag != "engine-index" {
		t.Fatalf("releases = %+v", got)
	}
	if len(got[0].Assets) != 2 || got[0].Assets[1].Size != 345 {
		t.Fatalf("assets = %+v", got[0].Assets)
	}
}

func TestReleasesReportsAnHTTPError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := c.Releases(context.Background()); err == nil {
		t.Fatal("Releases accepted a 404")
	}
}

func TestFetchReturnsTheAssetBody(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	})
	got, err := c.Fetch(context.Background(), c.BaseURL+"/asset")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("Fetch = %q", got)
	}
}

func TestFetchReportsANonSuccessStatus(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	if _, err := c.Fetch(context.Background(), c.BaseURL+"/asset"); err == nil {
		t.Fatal("Fetch accepted a 403")
	}
}

func TestPublishDelegatesToTheConfiguredUploader(t *testing.T) {
	var gotRepo, gotTag string
	var gotFiles []string
	c := NewClient("owner/repo")
	var gotForce bool
	c.Upload = func(ctx context.Context, repo, tag string, files []string, force bool) error {
		gotRepo, gotTag, gotFiles, gotForce = repo, tag, files, force
		return nil
	}
	if err := c.Publish(context.Background(), "engine-index", []string{"a.gz", "a.json"}, false); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if gotRepo != "owner/repo" || gotTag != "engine-index" || len(gotFiles) != 2 || gotForce {
		t.Fatalf("uploader saw %q %q %v force=%v", gotRepo, gotTag, gotFiles, gotForce)
	}
}

// Clobbering on every publish lets a routine upload destroy a concurrent
// publisher's asset.
func TestPublishOnlyClobbersWhenForced(t *testing.T) {
	for _, force := range []bool{false, true} {
		var got bool
		c := NewClient("owner/repo")
		c.Upload = func(ctx context.Context, repo, tag string, files []string, f bool) error {
			got = f
			return nil
		}
		if err := c.Publish(context.Background(), "t", []string{"a"}, force); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if got != force {
			t.Fatalf("force = %v, want %v", got, force)
		}
	}
}

func TestPublishPropagatesAnUploaderFailure(t *testing.T) {
	sentinel := errors.New("gh failed")
	c := NewClient("owner/repo")
	c.Upload = func(ctx context.Context, repo, tag string, files []string, force bool) error { return sentinel }
	if err := c.Publish(context.Background(), "t", []string{"a"}, false); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the uploader's error", err)
	}
}

func TestNewClientRejectsAMalformedRepo(t *testing.T) {
	c := NewClient("not-a-repo")
	if _, err := c.Releases(context.Background()); err == nil {
		t.Fatal("Releases accepted a repo without owner/name")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/indexshare/... -run 'Releases|Fetch|Publish|NewClient' -v`
Expected: FAIL to compile, `undefined: NewClient`.

- [ ] **Step 3: Write minimal implementation**

```go
package indexshare

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// DefaultTag is the release every engine index is published under. Engine
// identities are distinguished by asset name, not by tag, so that one
// release holds the whole catalogue and listing costs a single request.
const DefaultTag = "engine-index"

// Asset is one downloadable file on a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release is a published GitHub release.
type Release struct {
	Tag    string  `json:"tag_name"`
	Assets []Asset `json:"assets"`
}

// Uploader publishes files to a release. Injected so tests do not shell out.
type Uploader func(ctx context.Context, repo, tag string, files []string, force bool) error

// Client talks to a GitHub repository's releases. Reads go over plain HTTP
// because public release assets need no authentication, which keeps this
// module free of any third-party dependency; writes delegate to gh, which
// already owns token storage and refresh.
type Client struct {
	Repo    string
	BaseURL string
	HTTP    *http.Client
	Upload  Uploader
	// Authenticated reads a URL with credentials. Used when an anonymous read
	// is refused, which is what a private repository looks like from outside.
	Authenticated func(ctx context.Context, url string) ([]byte, error)
}

// MaxDownloadBytes bounds any single network body. The measured artifact is
// 332 MB, so 2 GB matches GitHub's own per-asset ceiling with room to spare
// while still refusing an unbounded stream.
const MaxDownloadBytes = 2 << 30

// NewClient builds a client for an "owner/name" repository.
func NewClient(repo string) *Client {
	return &Client{
		Repo:    repo,
		BaseURL: "https://api.github.com",
		HTTP:          &http.Client{Timeout: 30 * time.Minute},
		Upload:        GHUploader,
		Authenticated: GHFetch,
	}
}

func (c *Client) validRepo() error {
	if parts := strings.Split(c.Repo, "/"); len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("repository %q is not in owner/name form", c.Repo)
	}
	return nil
}

// Releases lists the repository's releases.
func (c *Client) Releases(ctx context.Context) ([]Release, error) {
	if err := c.validRepo(); err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/repos/%s/releases", strings.TrimRight(c.BaseURL, "/"), c.Repo)
	body, err := c.get(ctx, url)
	if err != nil {
		return nil, err
	}
	var out []Release
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode releases: %w", err)
	}
	return out, nil
}

// Fetch downloads one asset in full.
func (c *Client) Fetch(ctx context.Context, url string) ([]byte, error) {
	return c.get(ctx, url)
}

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		// Either it genuinely is not there, or the repository is private and
		// this request had no credentials. gh knows which.
		if c.Authenticated != nil {
			return c.Authenticated(ctx, url)
		}
		return nil, fmt.Errorf("request %s: %s (private repositories need gh; install and run gh auth login)", url, resp.Status)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("request %s: %s", url, resp.Status)
	}
	// Bound the read: this is a network body, and io.ReadAll lets the far end
	// decide how much memory to take.
	return io.ReadAll(io.LimitReader(resp.Body, MaxDownloadBytes))
}

// Publish uploads files to the given release tag. force decides whether an
// existing asset of the same name may be replaced.
func (c *Client) Publish(ctx context.Context, tag string, files []string, force bool) error {
	if err := c.validRepo(); err != nil {
		return err
	}
	upload := c.Upload
	if upload == nil {
		upload = GHUploader
	}
	return upload(ctx, c.Repo, tag, files, force)
}

// AssetNames returns every asset name on one release tag. Push checks this
// rather than the matched-candidate list, because a same-named asset whose
// discriminators disagree is filtered out of candidates and would otherwise
// be invisible to an overwrite check.
func (c *Client) AssetNames(ctx context.Context, tag string) (map[string]bool, error) {
	releases, err := c.Releases(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range releases {
		if r.Tag != tag {
			continue
		}
		for _, a := range r.Assets {
			out[a.Name] = true
		}
	}
	return out, nil
}

// GHUploader publishes through the gh CLI, creating the release on first use.
//
// --clobber is passed only for an explicit force. Passing it unconditionally
// would let a routine publish silently destroy a concurrent publisher's asset.
func GHUploader(ctx context.Context, repo, tag string, files []string, force bool) error {
	create := exec.CommandContext(ctx, "gh", "release", "create", tag,
		"--repo", repo, "--title", tag, "--notes", "Prebuilt clangd engine indexes.")
	if out, err := create.CombinedOutput(); err != nil && !strings.Contains(string(out), "already exists") {
		return fmt.Errorf("gh release create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	args := []string{"release", "upload", tag, "--repo", repo}
	if force {
		args = append(args, "--clobber")
	}
	upload := exec.CommandContext(ctx, "gh", append(args, files...)...)
	if out, err := upload.CombinedOutput(); err != nil {
		return fmt.Errorf("gh release upload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// GHFetch reads a URL through the gh CLI, which supplies the credentials an
// anonymous request lacks. Used as the fallback for private repositories: the
// spec promises a configurable target, and unauthenticated HTTP against a
// private repository returns 404 for the listing and every asset, so without
// this the private option does not exist.
func GHFetch(ctx context.Context, url string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", url)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api %s: %w", url, err)
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/indexshare/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/indexshare/release.go internal/indexshare/release_test.go
git commit -m "feat(indexshare): list and download releases over net/http, upload via gh"
```

---

## Task 12: List, Pull, and Push orchestration

**Files:**
- Create: `internal/indexshare/share.go`
- Test: `internal/indexshare/share_test.go`

**Interfaces:**
- Consumes: Tasks 10 and 11, plus `engineid.Identity`/`Match`/`Strength` and `idxrewrite.Denormalize`/`Normalize`/`AbsolutePathResidue`/`IndexFormatVersion`/`Roots`.
- Produces: `type Candidate struct { Manifest Manifest; Asset Asset; Strength engineid.Strength }`, `func List(ctx context.Context, c *Client, local engineid.Identity) ([]Candidate, error)`, `type PullRequest struct { Candidate Candidate; Roots idxrewrite.Roots; DestIndex string; LocalIndexFormat uint32 }`, `func Pull(ctx context.Context, c *Client, req PullRequest) error`, `type PushRequest struct { Index []byte; Manifest Manifest; Roots idxrewrite.Roots; StageDir string; Force bool }`, `func Push(ctx context.Context, c *Client, req PushRequest) error`, `var ErrAlreadyPublished error`, `var ErrLocalPathsRemain error`.

`Push` refuses when an artifact for the same identity already exists. That refusal is a likely experience for the second person to finish a long build, so its message says plainly that the existing artifact already matches and no upload is needed; read as a bare rejection it looks like a bug.

`Pull` writes through a temporary file and removes it on every error path, mirroring `engineindex.BuildIndex`.

- [ ] **Step 1: Write the failing test**

```go
package indexshare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/softdaddy-o/soft-ue-index/internal/engineid"
	"github.com/softdaddy-o/soft-ue-index/internal/idxrewrite"
)

// fixtureIndex builds a minimal but real clangd index carrying one engine URI.
func fixtureIndex(t *testing.T, uri string) []byte {
	t.Helper()
	c := &idxrewrite.Container{FormType: idxrewrite.FormType, Chunks: []idxrewrite.Chunk{
		{ID: idxrewrite.MetaChunkID, Data: []byte{20, 0, 0, 0}},
		{ID: idxrewrite.StringTableChunkID, Data: idxrewrite.EncodeStringTable([][]byte{[]byte("Sym"), []byte(uri)})},
		{ID: "refs", Data: []byte("refs")},
	}}
	return c.Marshal()
}

// catalogueServer serves a release whose assets are the given manifests and
// their artifacts.
func catalogueServer(t *testing.T, manifests []Manifest, artifacts map[string][]byte) *Client {
	t.Helper()
	mux := http.NewServeMux()
	var assets []Asset
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	for _, m := range manifests {
		body, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		name := ManifestName(m.EngineID)
		mux.HandleFunc("/dl/"+name, func(b []byte) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(b) }
		}(body))
		assets = append(assets, Asset{Name: name, URL: server.URL + "/dl/" + name, Size: int64(len(body))})
		if art, ok := artifacts[m.EngineID]; ok {
			an := ArtifactName(m.EngineID)
			mux.HandleFunc("/dl/"+an, func(b []byte) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(b) }
			}(art))
			assets = append(assets, Asset{Name: an, URL: server.URL + "/dl/" + an, Size: int64(len(art))})
		}
	}
	mux.HandleFunc("/repos/owner/repo/releases", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]Release{{Tag: DefaultTag, Assets: assets}})
	})
	c := NewClient("owner/repo")
	c.BaseURL = server.URL
	return c
}

func localIdentity() engineid.Identity {
	return engineid.Identity{
		Primary:        "5.8.1-cl55116800-UE5-0123456789abcdef",
		Structural:     "0123456789abcdef",
		Discriminators: []engineid.Discriminator{{Kind: "provenance", Value: "abc+def"}},
	}
}

func TestListReturnsOnlyMatchingIdentitiesWithTheirStrength(t *testing.T) {
	matching := sampleManifest()
	matching.EngineID = localIdentity().Primary
	matching.Discriminators = []engineid.Discriminator{{Kind: "provenance", Value: "abc+def"}}
	other := sampleManifest()
	other.EngineID = "5.7.0-cl1-UE5-ffffffffffffffff"
	c := catalogueServer(t, []Manifest{matching, other}, nil)

	got, err := List(context.Background(), c, localIdentity())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("candidates = %d, want 1", len(got))
	}
	if got[0].Strength != engineid.Confirmed {
		t.Fatalf("strength = %v, want Confirmed", got[0].Strength)
	}
}

func TestListDropsACandidateWhoseSharedDiscriminatorDisagrees(t *testing.T) {
	m := sampleManifest()
	m.EngineID = localIdentity().Primary
	m.Discriminators = []engineid.Discriminator{{Kind: "provenance", Value: "DIFFERENT"}}
	c := catalogueServer(t, []Manifest{m}, nil)
	got, err := List(context.Background(), c, localIdentity())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("candidates = %d, want 0; a disagreeing marker must reject", len(got))
	}
}

func TestPullInstallsAnIndexRetargetedToTheLocalEngineRoot(t *testing.T) {
	published, _, err := normalizeFixture(t, "file:///D:/Elpis_UE5.8/Engine/A.cpp", "D:/Elpis_UE5.8")
	if err != nil {
		t.Fatalf("normalize fixture: %v", err)
	}
	artifact, m, err := Pack(published, manifestFor(localIdentity().Primary))
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	c := catalogueServer(t, []Manifest{m}, map[string][]byte{m.EngineID: artifact})
	cands, err := List(context.Background(), c, localIdentity())
	if err != nil || len(cands) != 1 {
		t.Fatalf("List: %v, %d candidates", err, len(cands))
	}
	dest := filepath.Join(t.TempDir(), "engine.idx")
	err = Pull(context.Background(), c, PullRequest{
		Candidate:          cands[0],
		Roots:              idxrewrite.Roots{Engine: "C:/UE_5.8"},
		DestIndex:          dest,
		LocalIndexFormat:   20,
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	installed, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read installed index: %v", err)
	}
	if !strings.Contains(string(installed), "file:///C:/UE_5.8/Engine/A.cpp") {
		t.Fatal("installed index was not retargeted to the local engine root")
	}
	if strings.Contains(string(installed), idxrewrite.EnginePlaceholder) {
		t.Fatal("an ENGINE_PATH placeholder survived installation")
	}
}

func TestPullRefusesAnIncompatibleClangdMajor(t *testing.T) {
	published, _, _ := normalizeFixture(t, "file:///D:/E/Engine/A.cpp", "D:/E")
	artifact, m, _ := Pack(published, manifestFor(localIdentity().Primary))
	c := catalogueServer(t, []Manifest{m}, map[string][]byte{m.EngineID: artifact})
	cands, _ := List(context.Background(), c, localIdentity())
	dest := filepath.Join(t.TempDir(), "engine.idx")
	err := Pull(context.Background(), c, PullRequest{
		Candidate: cands[0], Roots: idxrewrite.Roots{Engine: "C:/UE_5.8"},
		DestIndex: dest, LocalIndexFormat: 21,
	})
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("err = %v, want ErrIncompatible", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Fatal("a refused pull left a file behind")
	}
}

func TestPushRefusesWhenTheSameIdentityIsAlreadyPublished(t *testing.T) {
	m := sampleManifest()
	m.EngineID = localIdentity().Primary
	m.Discriminators = localIdentity().Discriminators
	c := catalogueServer(t, []Manifest{m}, nil)
	c.Upload = func(ctx context.Context, repo, tag string, files []string, force bool) error {
		t.Fatal("Push uploaded despite an existing artifact")
		return nil
	}
	idx := fixtureIndex(t, "file:///D:/E/Engine/A.cpp")
	err := Push(context.Background(), c, PushRequest{
		Index: idx, Manifest: m, Roots: idxrewrite.Roots{Engine: "D:/E"}, StageDir: t.TempDir(),
	})
	if !errors.Is(err, ErrAlreadyPublished) {
		t.Fatalf("err = %v, want ErrAlreadyPublished", err)
	}
	if !strings.Contains(err.Error(), "already matches") {
		t.Fatalf("message %q does not explain that no upload is needed", err)
	}
}

func TestPushRefusesWhenLocalPathsWouldRemain(t *testing.T) {
	c := catalogueServer(t, nil, nil)
	c.Upload = func(ctx context.Context, repo, tag string, files []string, force bool) error {
		t.Fatal("Push uploaded an artifact carrying local paths")
		return nil
	}
	// The SDK root is not configured, so its URI cannot be normalised.
	idx := fixtureIndex(t, "file:///C:/Users/someone/sdk/x.h")
	err := Push(context.Background(), c, PushRequest{
		Index: idx, Manifest: manifestFor("id"), Roots: idxrewrite.Roots{Engine: "D:/E"}, StageDir: t.TempDir(),
	})
	if !errors.Is(err, ErrLocalPathsRemain) {
		t.Fatalf("err = %v, want ErrLocalPathsRemain", err)
	}
}

func TestPushUploadsBothAssetsWhenNothingIsPublished(t *testing.T) {
	c := catalogueServer(t, nil, nil)
	var uploaded []string
	var existedAtUploadTime []bool
	c.Upload = func(ctx context.Context, repo, tag string, files []string, force bool) error {
		uploaded = files
		for _, f := range files {
			_, err := os.Stat(f)
			existedAtUploadTime = append(existedAtUploadTime, err == nil)
		}
		return nil
	}
	idx := fixtureIndex(t, "file:///D:/E/Engine/A.cpp")
	m := manifestFor("5.8.1-cl1-UE5-aaaaaaaaaaaaaaaa")
	if err := Push(context.Background(), c, PushRequest{
		Index: idx, Manifest: m, Roots: idxrewrite.Roots{Engine: "D:/E"}, StageDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(uploaded) != 2 {
		t.Fatalf("uploaded %v, want the artifact and its manifest", uploaded)
	}
	for i, ok := range existedAtUploadTime {
		if !ok {
			t.Fatalf("staged file %s did not exist when upload ran", uploaded[i])
		}
	}
	// A 332 MB artifact left behind after every successful push is real
	// leakage, so staging is cleaned up once the upload returns.
	for _, f := range uploaded {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("staging file %s survived a successful push", f)
		}
	}
}
```

Add these two helpers to the same test file:

```go
func manifestFor(engineID string) Manifest {
	m := sampleManifest()
	m.EngineID = engineID
	m.Discriminators = localIdentity().Discriminators
	return m
}

func normalizeFixture(t *testing.T, uri, engineRoot string) ([]byte, idxrewrite.Result, error) {
	t.Helper()
	return idxrewrite.Normalize(fixtureIndex(t, uri), idxrewrite.Roots{Engine: engineRoot})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/indexshare/... -run 'List|Pull|Push' -v`
Expected: FAIL to compile, `undefined: List`.

- [ ] **Step 3: Write minimal implementation**

```go
package indexshare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/softdaddy-o/soft-ue-index/internal/engineid"
	"github.com/softdaddy-o/soft-ue-index/internal/idxrewrite"
)

// ErrAlreadyPublished reports that this engine identity is already covered.
var ErrAlreadyPublished = errors.New("engine index already published")

// ErrLocalPathsRemain reports an index that could not be fully normalised.
var ErrLocalPathsRemain = errors.New("index still contains local paths")

// Candidate is a published index graded against the local engine.
type Candidate struct {
	Manifest Manifest
	Asset    Asset
	Strength engineid.Strength
}

// List fetches every published manifest and returns those that match the
// local engine, strongest first.
func List(ctx context.Context, c *Client, local engineid.Identity) ([]Candidate, error) {
	releases, err := c.Releases(ctx)
	if err != nil {
		return nil, err
	}
	// Only the catalogue release. Merging assets across every release lets an
	// unrelated tag shadow a catalogue entry by name.
	byName := map[string]Asset{}
	for _, r := range releases {
		if r.Tag != DefaultTag {
			continue
		}
		for _, a := range r.Assets {
			if _, clash := byName[a.Name]; clash {
				return nil, fmt.Errorf("release %s has two assets named %s", r.Tag, a.Name)
			}
			byName[a.Name] = a
		}
	}
	var out []Candidate
	for name, asset := range byName {
		if !strings.HasSuffix(name, ".json") || !strings.HasPrefix(name, "engine-index-") {
			continue
		}
		body, err := c.Fetch(ctx, asset.URL)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", name, err)
		}
		var m Manifest
		if err := json.Unmarshal(body, &m); err != nil {
			continue
		}
		if err := m.Validate(); err != nil {
			continue
		}
		// The asset name is derived from the identity, so a name that does not
		// match the manifest it carries means one of the two is lying.
		if name != ManifestName(m.EngineID) {
			continue
		}
		strength := engineid.Match(local, m.Identity())
		if strength == engineid.NoMatch {
			continue
		}
		artifact, ok := byName[ArtifactName(m.EngineID)]
		if !ok {
			continue
		}
		out = append(out, Candidate{Manifest: m, Asset: artifact, Strength: strength})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Strength != out[j].Strength {
			return out[i].Strength > out[j].Strength
		}
		return out[i].Manifest.EngineID < out[j].Manifest.EngineID
	})
	return out, nil
}

// PullRequest describes one installation.
type PullRequest struct {
	Candidate          Candidate
	Roots              idxrewrite.Roots
	DestIndex        string
	// LocalIndexFormat is what this machine's clangd actually reads, as
	// measured by probing it, not inferred from a version number.
	LocalIndexFormat uint32
}

// Pull downloads a candidate, verifies it, retargets it to the local roots,
// and installs it atomically. Every failure path removes the staging file,
// mirroring engineindex.BuildIndex.
func Pull(ctx context.Context, c *Client, req PullRequest) (err error) {
	if req.Roots.Engine == "" {
		return errors.New("pull requires a local engine root")
	}
	artifact, err := c.Fetch(ctx, req.Candidate.Asset.URL)
	if err != nil {
		return err
	}
	idx, err := Unpack(artifact, req.Candidate.Manifest)
	if err != nil {
		return err
	}
	format, err := idxrewrite.IndexFormatVersion(idx)
	if err != nil {
		return err
	}
	if err := req.Candidate.Manifest.CheckCompatible(req.LocalIndexFormat, format); err != nil {
		return err
	}
	out, res, err := idxrewrite.Denormalize(idx, req.Roots)
	if err != nil {
		return err
	}
	if res.Rewritten[idxrewrite.EnginePlaceholder] == 0 {
		return fmt.Errorf("%w: no engine paths were retargeted", ErrCorrupt)
	}
	// Check the string table, not the whole file. Converting a 539 MB index to
	// a string doubles peak memory and would also match placeholder bytes that
	// happen to occur inside refs or symb.
	left, err := idxrewrite.PlaceholderResidue(out, idxrewrite.EnginePlaceholder)
	if err != nil {
		return err
	}
	if left > 0 {
		return fmt.Errorf("%w: %d %s placeholders survived installation", ErrCorrupt, left, idxrewrite.EnginePlaceholder)
	}
	if err := os.MkdirAll(filepath.Dir(req.DestIndex), 0o700); err != nil {
		return err
	}
	tmp := req.DestIndex + ".new"
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err = os.Rename(tmp, req.DestIndex); err != nil {
		return fmt.Errorf("move index into place: %w (close any clangd session holding %s and retry)", err, req.DestIndex)
	}
	return nil
}

// PushRequest describes one publication.
type PushRequest struct {
	Index    []byte
	Manifest Manifest
	Roots    idxrewrite.Roots
	StageDir string
	Force    bool
}

// Push normalises an index, refuses if any local path survives, and uploads
// it with its manifest.
func Push(ctx context.Context, c *Client, req PushRequest) error {
	normalized, rewriteResult, err := idxrewrite.Normalize(req.Index, req.Roots)
	if err != nil {
		return err
	}
	residue, samples, err := idxrewrite.AbsolutePathResidue(normalized)
	if err != nil {
		return err
	}
	if residue > 0 {
		return fmt.Errorf("%w: %d entries still hold a local path (for example %s); configure the missing toolchain roots and retry, or the artifact would publish them",
			ErrLocalPathsRemain, residue, strings.Join(samples, ", "))
	}
	// A URI under no known root keeps whatever absolute path it had. The
	// residue check above catches the drive-letter and UNC spellings, but the
	// count below is the direct statement of the invariant and catches forms
	// the scanner does not model.
	if rewriteResult.Unmatched > 0 {
		return fmt.Errorf("%w: %d file URIs fall under no configured root (for example %s); every root referenced by the index must be known before it can be published",
			ErrLocalPathsRemain, rewriteResult.Unmatched, strings.Join(rewriteResult.UnmatchedSamples, ", "))
	}
	// Existence is checked against raw asset names, not against matched
	// candidates. List deliberately drops entries whose discriminators
	// disagree -- exactly the entries a blind upload would destroy.
	taken, err := c.AssetNames(ctx, DefaultTag)
	if err != nil {
		return err
	}
	wanted := ArtifactName(req.Manifest.EngineID)
	if taken[wanted] && !req.Force {
		return fmt.Errorf("%w: %s already matches this engine, so there is nothing to upload; pass --force only if you intend to replace it",
			ErrAlreadyPublished, wanted)
	}
	format, err := idxrewrite.IndexFormatVersion(normalized)
	if err != nil {
		return err
	}
	m := req.Manifest
	m.SchemaVersion = SchemaVersion
	m.IndexFormat = format
	m.Placeholders = []string{
		idxrewrite.EnginePlaceholder, idxrewrite.SDKPlaceholder,
		idxrewrite.MSVCPlaceholder, idxrewrite.ClangPlaceholder,
	}
	m.URICount = 0
	for _, n := range rewriteResult.Rewritten {
		m.URICount += n
	}
	artifact, m, err := Pack(normalized, m)
	if err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return err
	}
	// Own the staging directory so a 332 MB artifact is not left behind after
	// a successful publish.
	stage, err := os.MkdirTemp(req.StageDir, "engine-index-push-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	artifactPath := filepath.Join(stage, ArtifactName(m.EngineID))
	manifestPath := filepath.Join(stage, ManifestName(m.EngineID))
	for _, path := range []string{artifactPath, manifestPath} {
		if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(stage)+string(filepath.Separator)) {
			return fmt.Errorf("refusing to stage outside %s: %s", stage, path)
		}
	}
	if err := os.WriteFile(artifactPath, artifact, 0o600); err != nil {
		return err
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(manifestPath, body, 0o600); err != nil {
		return err
	}
	return c.Publish(ctx, DefaultTag, []string{artifactPath, manifestPath}, req.Force)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/indexshare/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/indexshare/share.go internal/indexshare/share_test.go
git commit -m "feat(indexshare): list, pull, and push prebuilt engine indexes"
```

---

## Task 13: CLI parsing for engine-index

**Files:**
- Modify: `internal/cli/cli.go`
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: new `cli.Command` fields `Action string`, `Yes bool`, `Force bool`, `Repo string`, `NoRemote bool`; `engine-index <list|pull|push> <project>` accepted by `Parse`.

Existing fields and behaviour must not change: `Name`, `ProjectPath`, `ProjectName`, `JSON`, `EngineScope`, `DaemonAction`, `Child` all keep their meaning, and every current command keeps parsing exactly as before.

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/cli_test.go`:

```go
func TestParseEngineIndexRequiresAnActionAndProject(t *testing.T) {
	got, err := Parse([]string{"engine-index", "pull", "elpis"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Name != "engine-index" || got.Action != "pull" || got.ProjectName != "elpis" {
		t.Fatalf("command = %+v", got)
	}
}

func TestParseEngineIndexRejectsAnUnknownAction(t *testing.T) {
	if _, err := Parse([]string{"engine-index", "sync", "elpis"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("err = %v, want ErrUsage", err)
	}
}

func TestParseEngineIndexRejectsAMissingProject(t *testing.T) {
	if _, err := Parse([]string{"engine-index", "list"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("err = %v, want ErrUsage", err)
	}
}

func TestParseEngineIndexAcceptsItsFlags(t *testing.T) {
	got, err := Parse([]string{"engine-index", "push", "elpis", "--yes", "--force", "--repo=owner/name", "--json"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !got.Yes || !got.Force || got.Repo != "owner/name" || !got.JSON {
		t.Fatalf("command = %+v", got)
	}
}

func TestParseIndexEngineAcceptsNoRemote(t *testing.T) {
	got, err := Parse([]string{"index-engine", "elpis", "--no-remote"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !got.NoRemote {
		t.Fatalf("command = %+v", got)
	}
}

// Flags must stay scoped, the way --engine-scope and --child already are.
func TestParseScopesEngineIndexFlagsToTheirAction(t *testing.T) {
	for _, args := range [][]string{
		{"engine-index", "list", "elpis", "--force"},
		{"engine-index", "pull", "elpis", "--force"},
		{"engine-index", "list", "elpis", "--yes"},
		{"engine-index", "push", "elpis", "--repo="},
	} {
		if _, err := Parse(args); !errors.Is(err, ErrUsage) {
			t.Fatalf("Parse(%v) err = %v, want ErrUsage", args, err)
		}
	}
}

func TestParseRejectsEngineIndexFlagsOnOtherCommands(t *testing.T) {
	for _, args := range [][]string{
		{"list", "--yes"},
		{"generate", "elpis", "--force"},
		{"status", "elpis", "--repo=owner/name"},
		{"list", "--no-remote"},
	} {
		if _, err := Parse(args); !errors.Is(err, ErrUsage) {
			t.Fatalf("Parse(%v) err = %v, want ErrUsage", args, err)
		}
	}
}

func TestParseStillAcceptsEveryExistingCommandUnchanged(t *testing.T) {
	for _, args := range [][]string{
		{"list"}, {"doctor"}, {"watch"}, {"mcp"},
		{"add", "C:/p/x.uproject"},
		{"generate", "elpis"}, {"status", "elpis"}, {"remove", "elpis"},
		{"index-engine", "elpis"},
		{"daemon", "run"}, {"daemon", "status"}, {"daemon", "stop"},
	} {
		if _, err := Parse(args); err != nil {
			t.Fatalf("Parse(%v) = %v, want nil", args, err)
		}
	}
}
```

Ensure `errors` is imported in the test file.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/... -v`
Expected: FAIL, `got.Action undefined`.

- [ ] **Step 3: Modify cli.go**

Add the fields to `Command`:

```go
// Command is a parsed soft-ue-index command.
type Command struct {
	Name         string
	ProjectPath  string
	ProjectName  string
	JSON         bool
	EngineScope  string
	DaemonAction string
	Child        bool
	// Action is the sub-verb of a command that has one: engine-index's
	// list, pull, or push.
	Action string
	// Yes skips the confirmation prompt on engine-index pull and push.
	Yes bool
	// Force replaces an already-published artifact on engine-index push.
	Force bool
	// Repo overrides the owner/name repository engine-index talks to.
	Repo string
	// NoRemote suppresses index-engine's remote check, for scripted runs.
	NoRemote bool
}
```

In `Parse`'s flag loop, before the `positionals` append, add:

```go
		if arg == "--yes" {
			command.Yes = true
			continue
		}
		if arg == "--force" {
			command.Force = true
			continue
		}
		if arg == "--no-remote" {
			command.NoRemote = true
			continue
		}
		if strings.HasPrefix(arg, "--repo=") {
			command.Repo = strings.TrimPrefix(arg, "--repo=")
			continue
		}
```

Add `"strings"` to the imports.

In the command switch, add a case:

```go
	case "engine-index":
		if len(positionals) != 3 {
			return Command{}, usageError("engine-index requires an action (list, pull, or push) and a project name")
		}
		command.Action = positionals[1]
		switch command.Action {
		case "list", "pull", "push":
		default:
			return Command{}, usageError("unknown engine-index action %q, expected list, pull, or push", command.Action)
		}
		command.ProjectName = positionals[2]
```

After the existing `--engine-scope` and `--child` scope checks, add:

```go
	if (command.Yes || command.Force || command.Repo != "") && command.Name != "engine-index" {
		return Command{}, usageError("--yes, --force, and --repo only apply to engine-index")
	}
	if command.NoRemote && command.Name != "index-engine" {
		return Command{}, usageError("--no-remote only applies to index-engine")
	}
	// Scope per action too. A --force accepted on list reads as supported and
	// does nothing, which is worse than being rejected.
	if command.Force && command.Action != "push" {
		return Command{}, usageError("--force only applies to engine-index push")
	}
	if command.Yes && command.Action != "pull" && command.Action != "push" {
		return Command{}, usageError("--yes only applies to engine-index pull and push")
	}
	if command.Name == "engine-index" && command.Repo != "" && strings.TrimSpace(command.Repo) == "" {
		return Command{}, usageError("--repo requires an owner/name value")
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/... -v`
Expected: PASS, including the pre-existing tests.

- [ ] **Step 5: Commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/cli/cli.go internal/cli/cli_test.go
git commit -m "feat(cli): parse engine-index list, pull, and push"
```

---

## Task 14: App wiring, the remote check, and documentation

**Files:**
- Modify: `internal/app/app.go`
- Test: `internal/app/app_engineindexshare_test.go` (create)
- Modify: `README.md`

**Interfaces:**
- Consumes: everything above.
- Produces: `App.Run` handling `"engine-index"`; `type EngineIndexShareResult struct { Action string; EngineID string; Candidates []ShareCandidate; IndexPath string }`; `type ShareCandidate struct { EngineID, Strength string; SizeBytes int64 }`; deps fields `ShareClient func(repo string) *indexshare.Client` and `DefaultRepo string`.

The remote check is the point of the feature. Committing many hours of CPU without first asking whether the artifact already exists is the waste this removes.

Behaviour:

- `engine-index list` resolves the local identity, lists candidates, prints each with its match strength and size. Never mutates.
- `engine-index pull` picks the strongest candidate. If it is `PrimaryOnly`, it refuses unless `--yes`, because no shared discriminator confirmed it. Installs into the same cache path `index-engine` uses, then writes the `.clangd` fragment through the existing `engineindex.WriteFragment`.
- `engine-index push` normalises the built index and uploads it.
- `index-engine` runs the remote check first unless `--no-remote`: on a match it reports what it found and how to pull instead of building; with no match it proceeds, and after a successful build it reports that `engine-index push` would publish it.

`index-engine`'s remote check must never fail the build. A network error is reported and the build proceeds — being unable to reach GitHub is not a reason to refuse to index locally.

- [ ] **Step 1: Write the failing test**

```go
package app

import (
	"context"
	"strings"
	"testing"
)

// The remote check exists so that a user is told about an existing artifact
// before committing hours of CPU.
func TestIndexEngineReportsAMatchingRemoteIndexBeforeBuilding(t *testing.T) {
	env := newShareEnv(t)
	env.remote.publish(t, env.localEngineID(t))
	var built bool
	env.deps.IndexEngine = func(ctx context.Context, p registry.Project) (IndexEngineResult, error) {
		built = true
		return IndexEngineResult{}, nil
	}
	out := env.run(t, "index-engine", "elpis")
	if built {
		t.Fatal("index-engine built despite a matching remote index")
	}
	if !strings.Contains(out, "engine-index pull") {
		t.Fatalf("output %q does not tell the user how to fetch it", out)
	}
}

func TestIndexEngineStillBuildsWithNoRemoteMatch(t *testing.T) {
	env := newShareEnv(t)
	var built bool
	env.deps.IndexEngine = func(ctx context.Context, p registry.Project) (IndexEngineResult, error) {
		built = true
		return IndexEngineResult{EngineEntries: 1, IndexPath: "idx"}, nil
	}
	env.run(t, "index-engine", "elpis")
	if !built {
		t.Fatal("index-engine skipped the build with no remote match")
	}
}

// Being unable to reach GitHub is not a reason to refuse to index locally.
func TestIndexEngineBuildsAnywayWhenTheRemoteCheckFails(t *testing.T) {
	env := newShareEnv(t)
	env.remote.failEverything()
	var built bool
	env.deps.IndexEngine = func(ctx context.Context, p registry.Project) (IndexEngineResult, error) {
		built = true
		return IndexEngineResult{EngineEntries: 1, IndexPath: "idx"}, nil
	}
	out := env.run(t, "index-engine", "elpis")
	if !built {
		t.Fatal("a failing remote check blocked the local build")
	}
	if !strings.Contains(strings.ToLower(out), "remote") {
		t.Fatalf("output %q does not mention that the remote check failed", out)
	}
}

func TestIndexEngineSkipsTheRemoteCheckWithNoRemote(t *testing.T) {
	env := newShareEnv(t)
	env.remote.failIfCalled(t)
	env.deps.IndexEngine = func(ctx context.Context, p registry.Project) (IndexEngineResult, error) {
		return IndexEngineResult{EngineEntries: 1, IndexPath: "idx"}, nil
	}
	env.run(t, "index-engine", "elpis", "--no-remote")
}

func TestEngineIndexListPrintsCandidatesWithTheirStrength(t *testing.T) {
	env := newShareEnv(t)
	env.remote.publish(t, env.localEngineID(t))
	out := env.run(t, "engine-index", "list", "elpis")
	if !strings.Contains(out, "confirmed") {
		t.Fatalf("output %q does not show the match strength", out)
	}
}

// A candidate matched on the primary key alone was never confirmed by a
// shared marker, so installing it silently would be wrong.
func TestEngineIndexPullRefusesAnUnconfirmedCandidateWithoutYes(t *testing.T) {
	env := newShareEnv(t)
	env.remote.publishUnconfirmed(t, env.localEngineID(t))
	_, err := env.runErr(t, "engine-index", "pull", "elpis")
	if err == nil {
		t.Fatal("pull installed an unconfirmed candidate without --yes")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err %q does not name the flag that would proceed", err)
	}
}

func TestEngineIndexPullInstallsAndWritesTheFragment(t *testing.T) {
	env := newShareEnv(t)
	env.remote.publish(t, env.localEngineID(t))
	out := env.run(t, "engine-index", "pull", "elpis")
	if !strings.Contains(out, "installed") {
		t.Fatalf("output %q does not confirm installation", out)
	}
	env.assertFragmentWritten(t)
}

func TestEngineIndexPushReportsAnExistingArtifactAsNothingToDo(t *testing.T) {
	env := newShareEnv(t)
	env.remote.publish(t, env.localEngineID(t))
	env.buildLocalIndex(t)
	_, err := env.runErr(t, "engine-index", "push", "elpis")
	if err == nil || !strings.Contains(err.Error(), "already matches") {
		t.Fatalf("err = %v, want a message explaining no upload is needed", err)
	}
}
```

The snippets above use `registry.Project` directly; import
`github.com/softdaddy-o/soft-ue-index/internal/registry` the way
`app_indexengine_test.go` already does.

Read `internal/app/app_indexengine_test.go` and `internal/testutil/fixtures.go`
before writing the harness, and mirror their construction rather than
inventing a second style. The harness has this shape:

```go
type shareEnv struct {
	t          *testing.T
	deps       *Dependencies   // the struct app.New already takes
	out        *bytes.Buffer
	engineRoot string
	cacheDir   string
	remote     *fakeRemote
}

// newShareEnv builds a temp engine root, a registered project named
// "elpis" with a generated compilation database covering it, and an app
// whose ShareClient points at fakeRemote instead of GitHub.
func newShareEnv(t *testing.T) *shareEnv

// run executes one command and returns everything written to a.d.Output,
// failing the test if the command errors.
func (e *shareEnv) run(t *testing.T, args ...string) string

// runErr is run for the cases that are supposed to fail.
func (e *shareEnv) runErr(t *testing.T, args ...string) (string, error)

// localEngineID computes the identity the fixture project resolves to, so
// the fake remote can publish something that matches it.
func (e *shareEnv) localEngineID(t *testing.T) engineid.Identity

// buildLocalIndex writes a normalisable index at the cache path push reads.
func (e *shareEnv) buildLocalIndex(t *testing.T)

// assertFragmentWritten checks engineindex.FragmentPath(engineRoot) now
// names the installed index.
func (e *shareEnv) assertFragmentWritten(t *testing.T)

// fakeRemote is an httptest server plus the controls the tests need.
type fakeRemote struct {
	server  *httptest.Server
	calls   int
	failAll bool
}

// publish serves a confirmed-match manifest and artifact for id.
func (r *fakeRemote) publish(t *testing.T, id engineid.Identity)

// publishUnconfirmed serves a manifest matching only on the primary key,
// by publishing it with no discriminators.
func (r *fakeRemote) publishUnconfirmed(t *testing.T, id engineid.Identity)

// failEverything makes every request return 500.
func (r *fakeRemote) failEverything()

// failIfCalled fails the test if any request arrives, proving --no-remote
// really skipped the check rather than merely ignoring its result.
func (r *fakeRemote) failIfCalled(t *testing.T)
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/... -run 'EngineIndex|IndexEngineReports' -v`
Expected: FAIL to compile, `undefined: newShareEnv`, `d.ShareClient undefined`.

- [ ] **Step 3: Wire the app**

Add to the `Dependencies` struct (`internal/app/app.go:49`), alongside the existing `IndexEngine func(context.Context, registry.Project) (IndexEngineResult, error)` field:

```go
	// ShareClient builds the client engine-index talks to. A field so tests
	// can point it at a local server instead of GitHub.
	ShareClient func(repo string) *indexshare.Client
	// DefaultRepo is the owner/name repository used when --repo is absent.
	DefaultRepo string
	// RemoteCheck reports published indexes matching p, for index-engine to
	// offer before it spends hours building.
	//
	// It is a separate injectable rather than a call inside indexEngine
	// because every existing index-engine test injects only IndexEngine. If
	// the check were wired in directly, those tests would start resolving an
	// engine identity and reaching the network before their stub ran, and
	// their fixtures cannot do either. Tests that do not set this get the
	// no-op below.
	RemoteCheck func(context.Context, registry.Project) ([]indexshare.Candidate, error)
```

Default them in `New` (`internal/app/app.go:65`), next to the existing `if a.d.IndexEngine == nil` line:

```go
	if a.d.ShareClient == nil {
		a.d.ShareClient = indexshare.NewClient
	}
	if a.d.DefaultRepo == "" {
		a.d.DefaultRepo = "softdaddy-o/soft-ue-index"
	}
	if a.d.RemoteCheck == nil {
		a.d.RemoteCheck = a.remoteCheckReal
	}
```

Add the dispatch case in `Run`, next to `index-engine`:

```go
	case "engine-index":
		return a.engineIndexShare(ctx, command)
```

Implement `engineIndexShare`, a helper that resolves the local identity, and
the remote check. Put them below `indexEngineReal`. The identity resolution
reuses the same split the build already does, so the identity always
describes exactly the entries an index would cover:

```go
// localIdentity derives the sharing identity of p's engine: the same
// engine-scoped entry set index-engine would build from, hashed without any
// reference to where the engine is installed.
func (a *App) localIdentity(p registry.Project) (engineid.Identity, idxrewrite.Roots, []compdb.Entry, error) {
	raw, err := os.ReadFile(p.Generation.CompilationDatabase)
	if err != nil {
		return engineid.Identity{}, idxrewrite.Roots{}, nil, fmt.Errorf("read compilation database: %w", err)
	}
	var entries []compdb.Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return engineid.Identity{}, idxrewrite.Roots{}, nil, fmt.Errorf("decode compilation database: %w", err)
	}
	engineEntries, _ := engineindex.Split(entries, p.Engine.Root)
	if len(engineEntries) == 0 {
		return engineid.Identity{}, idxrewrite.Roots{}, nil, engineindex.ErrNoEngineEntries
	}
	// One reader for both passes. The database references far fewer response
	// files than entries -- 1,234 against 26,782 in the measured project --
	// so caching turns twenty reads of each file into one.
	read := engineid.CachingResponseReader()
	roots := toolchainRoots(p, engineEntries, read)
	id, err := engineid.Compute(p.Engine.Root, engineEntries, roots, read)
	return id, roots, engineEntries, err
}
```

`toolchainRoots` derives the SDK, MSVC, and clang resource roots from the
compilation database's own arguments. An unidentified root is not an error:
publishing refuses only if URIs actually reference it, and installing leaves
the placeholder in place.

```go
// toolchainRootMarkers maps a placeholder's root to the path segment that
// identifies it. The root is the path truncated just after that segment, so
// every machine agrees on the same depth.
var toolchainRootMarkers = []struct{ field, marker string }{
	{"sdk", "/windows kits/"},
	{"msvc", "/microsoft visual studio/"},
	{"clang", "/lib/clang/"},
}

// toolchainRoots finds the SDK, MSVC, and clang resource directories so their
// URIs can be normalised too. They are about 1.1% of a real index's URIs, but
// a published artifact carrying them would also carry the publisher's
// username and install layout.
//
// This must expand response files. In the real database every entry is
// [compiler, @response-file, source]: scanning Arguments alone finds no
// include paths at all and silently returns three empty roots, which then
// fails publishing with a confusing residue error instead of here.
func toolchainRoots(p registry.Project, entries []compdb.Entry, read engineid.ResponseReader) idxrewrite.Roots {
	roots := idxrewrite.Roots{Engine: filepath.ToSlash(p.Engine.Root)}
	assign := func(field, value string) {
		switch field {
		case "sdk":
			if roots.SDK == "" {
				roots.SDK = value
			}
		case "msvc":
			if roots.MSVC == "" {
				roots.MSVC = value
			}
		case "clang":
			if roots.Clang == "" {
				roots.Clang = value
			}
		}
	}
	for _, e := range entries {
		args, _, err := engineid.ExpandArguments(e, read)
		if err != nil {
			continue
		}
		for _, arg := range args {
			slashed := strings.ReplaceAll(arg, `\`, "/")
			lowered := strings.ToLower(slashed)
			for _, m := range toolchainRootMarkers {
				at := strings.Index(lowered, m.marker)
				if at < 0 {
					continue
				}
				// Find where the path itself starts, which is not where the
				// token starts: "/I" and quoting put the drive letter well
				// inside the token.
				start := idxrewrite.AbsolutePathIndex(slashed)
				if start < 0 || start > at {
					continue
				}
				assign(m.field, slashed[start:at+len(m.marker)-1])
			}
		}
		if roots.SDK != "" && roots.MSVC != "" && roots.Clang != "" {
			break
		}
	}
	if roots.Clang == "" {
		roots.Clang = clangResourceRoot(p.Toolchain.ClangdPath)
	}
	return roots
}

// clangResourceRoot locates the resource directory next to a clangd binary,
// returning "" when it is not where a standard layout puts it.
//
// It stops at lib/clang, not at lib/clang/<version>, so that it agrees with
// the marker-based path above. Two code paths that pick different depths for
// the same directory normalise the same index differently, which shows up
// only as a mysteriously unmatched identity.
func clangResourceRoot(clangdPath string) string {
	if clangdPath == "" {
		return ""
	}
	base := filepath.Join(filepath.Dir(filepath.Dir(clangdPath)), "lib", "clang")
	info, err := os.Stat(base)
	if err != nil || !info.IsDir() {
		return ""
	}
	return filepath.ToSlash(base)
}
```

The manifest `push` publishes is built from the identity, never by hand, so
that no field can drift from what was actually hashed:

```go
// shareManifest renders an identity as a publishable manifest. Only derived
// values travel: no path, username, or raw marker content.
func shareManifest(id engineid.Identity, clangdVersion string, engineEntries int, now time.Time) indexshare.Manifest {
	return indexshare.Manifest{
		SchemaVersion:  indexshare.SchemaVersion,
		EngineID:       id.Primary,
		Version:        id.Version,
		Structural:     id.Structural,
		Discriminators: id.Discriminators,
		ClangdVersion:  clangdVersion,
		EngineEntries:  engineEntries,
		PublishedAt:    now.UTC(),
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -v`
Expected: PASS across every package.

- [ ] **Step 5: Update the README**

Extend the existing "Prebuilt engine index (optional)" section with a
"Sharing an engine index" subsection covering: the three commands, that
`index-engine` checks the remote first and how to disable that, what a
published artifact contains and that it holds no local paths, the clangd
major-version requirement, and that the upload target is configurable for
teams publishing privately. Keep the existing note that the mechanism was
verified on a synthetic fixture at small scale, and add that placeholder
round-tripping is likewise covered by tests rather than by a full-scale
published artifact until one exists.

- [ ] **Step 6: Final gate and commit**

```bash
gofmt -l ./... && go build ./... && go vet ./... && go test ./...
git add internal/app/app.go internal/app/app_engineindexshare_test.go README.md
git commit -m "feat(app): wire engine-index sharing and check the remote before building"
```

---

## Verification beyond the test suite

The unit tests cannot prove the clangd-facing half. Before this is called
done, run the end-to-end check from the spec by hand:

1. Take a real built index. Normalise it, confirm `AbsolutePathResidue`
   returns zero, and confirm `symb`, `refs`, and `rela` are byte-identical
   to the original.
2. De-normalise it to a *different* engine root that holds the same tree.
3. Point clangd at the result through the generated `.clangd` fragment and
   confirm that a symbol which only the external index can serve resolves,
   using the methodology that validated the original external-index work.
   A clean parse log is not evidence; an absolute-style config compiles
   cleanly and silently matches nothing.

Report the result plainly. If step 3 was not run, say so rather than letting
a green suite imply it.
