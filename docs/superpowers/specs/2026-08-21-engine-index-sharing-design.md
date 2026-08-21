# Sharing prebuilt engine indexes over GitHub

Date: 2026-08-21
Status: approved design, not yet implemented

## Problem

`index-engine` builds a monolithic clangd index for an engine tree so that
clangd stops re-indexing Engine/Plugins on every machine. It works, but it is
expensive. On a real UE 5.8 project the build took **about 46 hours** for
**24,996 translation units** and produced a **539 MB** `.idx`.

Every engineer on a team with the same engine pays that cost again. So does
every fresh machine. The index is a pure function of the engine tree and the
compile flags, so this is duplicated work, not per-user work.

This document specifies how one machine publishes an index and another machine
finds and installs it.

## Verified constraints

These were measured against a real 539 MB index and against clangd's source.
They are recorded here because each one rules out an approach that looks
obviously correct until you check.

### clangd cannot relocate a monolithic index

`Index.External.MountPoint` does **not** rebase paths for the on-disk `File`
case. In `loadExternalIndex` the `File` branch passes only `External.Location`
to `loadIndex`; `MountPoint` reaches nothing but a `log()` call. Only the
`Server` (remote index) branch performs path translation, via
`remote::getClient(Location, MountPoint)`.

### clangd-indexer cannot emit relative paths

Its only flags are `--format` and `--query-driver`. Paths are stored through
`getCanonicalPath`, which yields absolute paths.

### Symlink and junction tricks do not work

`getCanonicalPath` resolves to the real path on purpose. Its comment states
that clangd always wants the real file path rather than the symlink path.
Building against a canonical alias path therefore still records the real path.

### But the index is trivially rewritable

Measured chunk layout of a real index:

```
RIFF/CdIx
  meta      4 bytes      index format version (20 for clangd 20.1.8)
  stri     59 MB         string table
  symb     81 MB
  refs    395 MB
  rela    3.4 MB
  srcs      0 bytes
```

- All strings live in one deduplicated table in `stri`. Every other chunk
  refers to them by varint index, so rewriting a string does not disturb any
  other chunk.
- In the measured index the table was stored **uncompressed** and
  NUL-separated, so no compression round-trip is required.
- Of 1,360,749 string entries, **66,484** are `file:///` URIs. Everything else
  is symbol names and other content.
- **Zero** absolute paths appear outside those URIs. This was checked by
  scanning the `stri` chunk in isolation. Scanning the whole file instead
  produces false positives, because random bytes in `refs` and `symb` resemble
  drive-letter paths.

Rewriting the URI set is therefore both sufficient and complete.

## Design

### 1. Canonical placeholders

An index is normalised at publish time and de-normalised at install time.
Publishing rewrites every absolute root to a fixed placeholder:

| Role | Placeholder |
| --- | --- |
| Engine tree | `file:///ENGINE_PATH/...` |
| Windows SDK | `file:///SDK_PATH/...` |
| MSVC toolchain | `file:///MSVC_PATH/...` |
| clang resource dir | `file:///CLANG_PATH/...` |

Measured distribution across those roots in a real index:

| Root | URIs | Share |
| --- | ---: | ---: |
| Engine | 65,737 | 98.9% |
| Windows SDK | 431 | 0.6% |
| clang resource dir | 159 | 0.2% |
| MSVC | 157 | 0.2% |

Normalising at publish time, rather than only rewriting at install time, buys
three things:

1. The installer always substitutes the same known prefixes, so it never has
   to trust a path recorded in the manifest.
2. The published artifact contains no local paths at all, so it cannot leak a
   username or an internal directory layout.
3. It creates a checkable invariant: a correctly published artifact has zero
   drive-letter paths in `stri`. Publishing refuses if any remain.

URIs are percent-encoded, so the rewriter operates on encoded forms and must
not naively decode them first.

On install, `ENGINE_PATH` **must** resolve to the local engine root. The other
three are best-effort: the installer substitutes the local toolchain when it
can identify it, and otherwise leaves the placeholder in place. An unresolved
placeholder costs only go-to-definition on that symbol, affecting about 1.1%
of entries, and the project's own live index generally covers those symbols
anyway.

### 2. Engine identity

The existing content key hashes the engine root path and local absolute file
paths. That is correct for a local cache and unusable for sharing: two people
with the same engine at different paths compute different keys, so a lookup
could never match. Sharing needs a separate, path-independent identity.

`Engine.Version` in the registry is also insufficient on its own. It records
only major and minor, so two different patch releases or two different
changelists of the same minor version collide.

Identity is therefore layered: a **primary key** that every engine can produce,
plus **discriminators** that only some distributions carry.

Primary key, always computable:

- `Engine/Build/Build.version`: major, minor, patch, `CompatibleChangelist`,
  `BranchName`, `BuildId`, `IsLicenseeVersion`.
- A structural hash over the set of engine-relative source paths and the
  **path-normalised** compile arguments.

The word *normalised* carries weight there. Compile arguments embed absolute
include directories. Hashing them raw would make the identity path-dependent
again and defeat the whole scheme. Include paths are rewritten to the same
placeholders as above before hashing.

Discriminators, recorded when present:

| Marker | Available when | Strength |
| --- | --- | --- |
| Hash of a per-file checksum manifest shipped with the distribution | Distributions that ship one | Exact: pins actual file contents |
| Upstream commit hash plus the checksum of any applied patch | Distributions that record their provenance | Near-exact |
| GUID in `Engine/Build/InstalledBuild.txt` | Installed builds whose GUID is part of the payload | Per-build |
| Commit hash in `Engine/Build/Commit.gitdeps.xml` | Source distributions fetched from GitHub | Per-commit |

A commit hash alone is not an identity. A distribution may apply patches on top
of an upstream commit, so a commit and a patch checksum must be recorded and
compared as a pair.

Discriminator extraction is **pluggable**. Each distribution format gets a
small adapter that parses whatever marker files it has and returns derived
values. Adapters return only derived values, never raw file contents, because
marker files routinely contain internal build paths that must not reach a
published manifest.

Match policy:

- Candidates must agree on the primary key.
- If both sides carry the same discriminator, it must match, or the candidate
  is rejected.
- If only one side carries a discriminator, the candidate is offered with an
  explicit warning and requires confirmation.

### 3. Artifact layout

A published release carries two files per engine identity:

```
engine-index-<engineId>.idx.gz     compressed index
engine-index-<engineId>.json       manifest
```

gzip on the measured index gave 539 MB to 332 MB in 16 seconds. GitHub's
per-asset limit is 2 GB, so this fits with headroom.

The manifest records: schema version, engine identity primary key, all
discriminators found, the clangd index format version, the clangd version used
to build it, the placeholder set used, the SHA-256 of the compressed artifact,
the SHA-256 of the normalised index, entry counts, and the publication time.
It records no local paths.

### 4. Command surface

```
soft-ue-index engine-index list           # remote indexes matching this engine
soft-ue-index engine-index pull [--yes]   # download, de-normalise, install
soft-ue-index engine-index push [--yes]   # normalise, verify, upload
```

`list` and `pull` resolve the local engine identity first and report what
matched and at what strength. `push` refuses when an artifact for the same
identity already exists, unless forced.

`index-engine` gains a remote check before it starts building. Committing 46
hours of CPU without first asking whether the artifact already exists is the
central waste this feature removes. When a match exists it offers to pull
instead; when a build finishes and nothing was published for that identity, it
offers to push. `--no-remote` disables both prompts for scripted use.

### 5. Transport

| Direction | Mechanism | Rationale |
| --- | --- | --- |
| Download | standard library `net/http` | Public release assets need no auth, so no new dependency |
| Upload | shell out to `gh release upload` | Avoids implementing token storage and refresh |

The module currently depends only on go-winio, fsnotify, the MCP SDK, and
`golang.org/x/sys`. This split adds no third-party dependency.

The upload target is configurable so a team can publish to a private
repository instead of a public one.

### 6. Integrity and failure handling

- Install refuses unless the manifest's clangd **major** version equals the
  local clangd's major version. The manifest also records the index format
  version read from the `meta` chunk, and install refuses if the artifact's
  actual `meta` value disagrees with what its manifest claims. Deriving the
  local clangd's expected format version directly is not practical at install
  time, so the clangd major version is the operative gate and `meta` is the
  tamper and corruption check. A mismatch is a hard refusal, not a warning:
  clangd owns this format and may change it, and silently loading a stale
  format is the failure mode most likely to waste a user's afternoon.
- SHA-256 of the compressed artifact is verified before decompression.
- After de-normalisation the installer asserts that no `ENGINE_PATH`
  placeholder remains. A residue means the rewrite was incomplete, and the
  install fails rather than installing a half-rewritten index.
- Before publishing, the normaliser asserts zero drive-letter paths in `stri`.
- Downloads stage to a temporary file and are removed on any error path,
  matching the existing `defer`-based cleanup in `BuildIndex`.

### 7. Non-goals

- Rebuilding or patching an index whose engine differs. Identity mismatch is a
  refusal, never a partial merge.
- Running a remote index server. clangd supports it and it does relocate paths,
  but it is a persistent network service and a different product than
  publishing a file and fetching a file.
- Hosting. This feature publishes to a repository the user already controls.
- Incremental or delta updates. A new engine identity means a new artifact.

## Testing

Behaviour that this feature gets wrong silently must be tested against real
bytes, not mocks.

- Round-trip: normalise a fixture index, assert zero absolute paths, then
  de-normalise to a different root and assert the URI set matches expectation
  exactly.
- Rewriting must not disturb other chunks: assert `symb`, `refs`, and `rela`
  are byte-identical before and after a normalise and de-normalise cycle.
- Format guard: an artifact whose manifest names a different clangd major
  version must be refused, and so must one whose actual `meta` value disagrees
  with its manifest.
- Identity is path-independent: the same engine tree registered at two
  different roots must produce the same primary key, while the existing content
  key still differs. This is the regression that motivates the whole identity
  layer.
- Identity is discriminating: a changed patch checksum, or a changed structural
  hash, must produce a different identity.
- Percent-encoded roots survive rewriting.
- Publishing refuses when a local path would remain.

The clangd-facing behaviour cannot be proven by unit tests alone. An end-to-end
check must confirm that an index normalised, published, fetched, and
de-normalised to a different root actually resolves symbols through clangd,
using the methodology that validated the original external-index work: verify
against a symbol that only the external index can serve, rather than trusting a
clean parse log.
