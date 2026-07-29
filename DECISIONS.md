# DECISIONS

One entry per non-obvious design decision, with the alternative rejected and why.
Part 2 is the wire-format write-up: every claim cites the source file and function it
was derived from. All paths are relative to `tools/fixturegen/node_modules/`, pinned at
`yjs@13.6.31`, `y-protocols@1.0.7`, `lib0@0.2.117`, `y-websocket@3.0.0`.

---

## Part 1 — Decisions

### D1. Go module path is `github.com/mesutokul/ycollab`
The directory is named `CrdtServer`, the project is named `ycollab`. Rejected
`github.com/mesutokul/crdtserver` — the import path should read like the product, and the
directory name is an accident of where the repo was cloned.

### D2. Fixture tooling depends on real `yjs`, pinned to exact versions
`tools/fixturegen` installs `yjs`, `y-protocols`, `lib0` with `--save-exact`. Rejected
caret ranges: the fixtures are checked in as binary, so a silent minor-version bump that
changes encoding would show up as a mysterious Go test failure rather than as a dependency
change in review.

`y-websocket@3.0.0` is a devDependency of the same project. The outer websocket framing
(`messageSync = 0`, `messageAwareness = 1`, `messageAuth = 2`, `messageQueryAwareness = 3`) is
defined by `y-websocket`, not by `y-protocols`, and the brief says derive the protocol from
source rather than memory. Since Phase 2 it also does real work: `tools/soak` drives its
`WebsocketProvider` against the Go server, and `web/` pins the same version. Node tooling, not
a Go dependency.

### D3. `node_modules` is gitignored, `package-lock.json` is committed
The brief calls `node_modules` the authoritative spec source, which argues for committing it.
Rejected: ~10 MB of vendored JS in a Go repo, and the lockfile plus exact versions already
make the tree reproducible with `npm ci`. The fixture `manifest.json` also records the
versions the committed binaries were generated with.

### D4. Fixture client IDs are hardcoded
`Y.Doc` assigns a random 32-bit `clientID`, so every regeneration would produce different
bytes and a noisy diff. `createHarness().doc(label, clientID)` overwrites `doc.clientID`
before the first operation. Verified: two consecutive `node generate.mjs` runs produce 221
byte-identical files.

### D5. One `node_modules`; the verifier imports through the generator
`tools/verify/apply.mjs` gets `yjs` via `import { Y } from '../fixturegen/dump.mjs'` instead of
having its own `npm install`. Rejected a second install: the verifier's whole job is to prove
Go bytes are accepted by *the same* Yjs that produced the fixtures. Two installs could drift
apart and turn a real incompatibility into a passing test.

### D6. Fixtures carry both bytes and a JSON description of those bytes
Each scenario directory holds the binary (`state.bin`, `update-NNN.bin`, `diff.bin`, framed
`msg-*.bin`) *and* `expected.json` / `updates.json`, which describe every struct in wire order
(`id`, `origin`, `rightOrigin`, `parent`, `parentSub`, content ref and value) as decoded by
`Y.decodeUpdate` (`yjs/src/utils/updates.js:139`). Rejected bytes-only fixtures: when the Go
decoder disagrees, a struct-level JSON diff points at the field, whereas a byte diff points at
an offset.

### D7. The generator fails if fixture coverage regresses
`assertCoverage()` decodes every emitted `.bin` and requires content refs 1, 3, 4, 5, 6, 7, 8, 9
and struct kinds `Item`, `GC`, `Skip` to be present. Rejected trusting the scenario list: a
scenario edit that quietly stops producing `GC` structs would leave a green test suite covering
less than it claims.

Content ref 2 (`ContentJSON`) has no fixture: current Yjs never emits it (`ContentAny`
superseded it), so it cannot be produced through the public API. The Go decoder must still
support it — that will be covered by a hand-built byte sequence in the Go tests.

### D8. Decode every content ref; expose only `YText` and `YMap` in Go
Section 8 puts `YXmlFragment` out of scope, but the Phase 2 acceptance criterion is TipTap,
and TipTap (`y-prosemirror`) stores its document as `Y.XmlFragment` of `Y.XmlElement` /
`Y.XmlText`. So `ContentType` with type refs 3/4/6, plus `ContentFormat` and `ContentEmbed`,
*will* arrive on the wire whatever we decide.

Resolution (confirmed with you): `internal/crdt` decodes and re-encodes every content ref
byte-exactly, but the typed Go API stays `YText` + `YMap` as the brief specifies. The server
never needs type semantics — it integrates structs and computes diffs. The `xml-prosemirror`
fixture is verified by round-trip and state-vector equality, not by a Go XmlFragment API.
Rejected: implementing a Go XmlFragment API now (more Phase 1 surface for no server benefit);
also rejected swapping the demo to Monaco (loses the richer demo and does not remove the
decoding requirement anyway).

### D9. The import-purity test walks non-test files only
Section 4 requires zero third-party imports in `internal/crdt`, and also allows `gopter` for
the Phase 1 property tests that live in that directory. Resolution: the purity test inspects
non-`_test.go` files, and property tests live in external test packages (`package crdt_test`).
This keeps the shipped package standard-library-only, which is the actual goal.

### D10. Update format v1 only
Yjs has two encodings: v1 (`UpdateEncoderV1`) and v2 (`UpdateEncoderV2`, run-length /
delta-compressed columns, `yjs/src/utils/UpdateEncoder.js:162`). `y-websocket` and
`y-protocols/sync` use v1 exclusively (`sync.js:61` calls `Y.encodeStateAsUpdate`, which is v1
per `yjs/src/utils/encoding.js:555`). Implementing v2 would add several column encoders
(`IntDiffOptRleEncoder`, `UintOptRleEncoder`, `StringEncoder`) for zero wire benefit. Out of
scope per section 8, and confirmed by the source: v1 is what browsers send.

### D11. Each fixture records its `gc` setting
Applying a fixture's `state.bin` into a document with a different `gc` flag collapses
tombstones differently (`Y.Doc({gc})` controls whether deleted items become `GC` structs), so
`expected.json` carries `gc` and the verifier constructs its document accordingly. Without
this, `text-delete` (gc off, `ContentDeleted` preserved) and `gc-and-skip` (gc on, structs
collapsed) cannot both be checked by the same harness.

### D12. `lib0.Encoder` collects a sticky error instead of returning one per write
Every write would otherwise return an `error` that no call site can do anything useful with,
and the CRDT encoder makes thousands of them. Instead the first failure is recorded, later
writes become no-ops, and callers check `Err()` once — the `bufio.Writer` pattern. The only
failure a writer can produce is a value above `MaxSafeInteger`, which is a programming error,
not a runtime condition.

### D13. Go is deliberately stricter than lib0 on truncated input
`lib0/decoding.js` `readVarInt` reads past the end of its buffer, gets `undefined`, and
returns `0` instead of throwing (`readVarUint` does throw). Go returns `ErrUnexpectedEOF`.
Rejected bug-compatibility: silently accepting truncated frames from the network is how a
server ends up integrating garbage. The divergence is recorded as a `goStricter` vector in
`testdata/fixtures/lib0/vectors.json`, and the generator asserts lib0 still behaves that way,
so if a future lib0 fixes it we find out.

### D14. `-race` runs locally via MinGW-w64
`go test -race` on windows/amd64 needs `CGO_ENABLED=1` and a C compiler, and this machine had
neither. Resolved by installing WinLibs MinGW-w64 (`winget install
BrechtSanders.WinLibs.POSIX.UCRT`, gcc 16.1.0) and `go env -w CGO_ENABLED=1`. The installer
puts its `mingw64\bin` on the persistent user PATH, so new shells pick it up; shells started
before the install need to be restarted. Rejected running the race build only in Docker/WSL:
the feedback loop matters most in Phase 2 where the room actor and gateway are written, and a
container round trip per test run would discourage running it.

### D15. Decoded `any` objects keep their key order
`writeAny` writes object keys in `Object.keys` order (`lib0/encoding.js:590`) and Yjs stores
the resulting bytes verbatim inside `ContentAny`, so a value that is decoded and re-encoded
must reproduce that order. A Go `map[string]any` cannot, so `ReadAny` returns an ordered
`*lib0.Object` (a `[]Field` slice with `Get`/`Set`). `WriteAny` still accepts a plain map for
values we construct ourselves, writing its keys sorted so our own output stays deterministic.
Rejected returning `map[string]any` for ergonomics: it would make re-encoding a document
byte-unstable, which is exactly the property Phase 1 is verified on.

### D16. NaN is canonicalised on write
Go's `math.NaN()` is `0x7FF8000000000001`; V8 produces `0x7FF8000000000000`. Both are NaN, but
only the second matches the bytes a browser writes, so `WriteAny` writes the canonical pattern
for every NaN and drops the payload bits. JavaScript cannot express a NaN payload through
`writeAny`, so nothing is lost. Found by the golden vector, not by reading the source —
`isFloat32(NaN)` is `false` because it compares `NaN === NaN`, which is what puts NaN on the
float64 branch in the first place while `Infinity` goes to float32.

### D17. `any` decoding is depth- and length-guarded
`readAny` is recursive and takes lengths from the input; a hostile 5-byte frame can otherwise
ask for a 2^31-element array. Go caps nesting at 128 (`ErrDepthExceeded`) and refuses any
length prefix larger than the remaining input before allocating. Yjs has no such limits;
this only rejects input no real client produces.

### D18. Encoding canonicalises client order; decoding accepts any order
Yjs writes client blocks, delete-set clients and state-vector clients descending, but its
decoder accepts any order. Go decodes any order and always *writes* the canonical one, so
`Encode` is a fixed point: encoding an already-encoded update never changes it again. The
fixture test asserts byte identity for the 132 updates Yjs actually produced; the fuzz target
asserts idempotency for everything else. Rejected preserving the input order: it would mean
carrying an ordering field through the whole data model to be bug-compatible with input no
client sends.

### D19. Values that decode must be re-encodable
`lib0`'s `readVarUint` can return values above 2^53−1 (its range check runs only when a
continuation byte follows) while `writeVarUint` refuses them. That gap let a decoded client id
become an update that could not be written back — found by the fuzzer as a silently truncated
re-encode. Client ids, clocks and IDs are now range-checked on the way in, and `Update.Encode`
/ `EncodeStateVector` return an error instead of a short buffer, since `lib0.Encoder` drops
every write after its first failure.

### D20. Structs whose info byte carries meaningless flags are rejected
The flag bits for origin, rightOrigin and parentSub have no meaning on a `GC` or `Skip`
struct. Yjs ignores them and writes zeros back, i.e. it silently rewrites the struct. Go
rejects the update instead (`ErrCorruptUpdate`), so that what this server relays is always
what it received. Also found by the fuzzer.

### D21. Embed, format and legacy-JSON values are kept as raw JSON text
Yjs stores these as `JSON.stringify` output and re-serialises them on write. Go cannot
reproduce JavaScript's key order from a parsed value, so a decode-then-re-encode would change
bytes that clients hash and merge. The raw string is stored instead and parsed only when a
typed accessor needs the value. Same reasoning as [D15] one level up.

### D22. Undeliverable updates are held, not rejected
An update whose dependencies are missing goes into a pending list and is retried after every
later integration, mirroring Yjs's `restStructs`/`pendingStructs`. Rejecting it would lose the
edit permanently, and a Yjs client legitimately sends an update before the one it builds on
when a websocket reconnects mid-stream. The same applies to delete-set ranges naming clocks we
have not received (`pendingDeletes`). `Doc.PendingCount` exposes the backlog so Phase 2 can
alarm on it.

### D23. Type lengths are computed by walking, not cached
Yjs maintains `AbstractType._length` incrementally. Go recomputes it, because the server does
not edit documents in a loop - it integrates updates and serialises them - and an incremental
counter is one more thing that can silently drift out of sync with the item list. Revisit if
Phase 6 benchmarks show it.

### D24. Go re-encodes documents byte-identically to Yjs
Not a decision so much as a checked property: applying each fixture's `state.bin` into a Go
`Doc` and calling `EncodeStateAsUpdate(nil)` reproduces the original file byte for byte, for
all 13 scenarios, and `tools/verify/apply.mjs` accepts every one of those outputs in a real
`Y.Doc`. `cmd/ycollab-dump` regenerates the outputs for that check.

### D25. `internal/protocol` is pure bytes
The framing layer takes and returns byte slices and touches no socket, no clock beyond the
timestamp callers pass in, and no room state. That is what lets every message type be checked
against the committed `msg-*.bin` fixtures that real `y-protocols` produced, in both
directions. Rejected: folding the framing into the gateway, where it would only be reachable
through a live connection and the fixtures would go unused.

### D26. Awareness state is relayed verbatim, never parsed
A client's awareness state is kept as the raw JSON string it sent. The server has no business
knowing what a cursor is, and re-serialising the value would let key order or number formatting
drift on the way through, for no benefit. Same reasoning as D21 for embed and format content.
Rejected: decoding to a Go map, which would also mean re-encoding on every fanout.

### D27. Updates are relayed as the author's bytes
When a client sends an update the room applies it and then broadcasts **the bytes it received**,
rather than re-encoding a delta from its own document. Two reasons: peers get exactly what the
author produced, and an update whose dependencies we are still missing is relayed rather than
stuck behind our own integration state (which is what would happen if we broadcast a diff from
our store — see [C5]). Yjs's own server broadcasts the update its transaction emitted, which is
a merged form; ours is closer to the wire the client wrote. Duplicate structs on the wire are
idempotent, so the cost is bandwidth, not correctness.

### D28. A sender never receives its own update back
The reference `y-websocket` server broadcasts to every connection including the author. We skip
the author: it already has the update by construction, and echoing doubles the fanout of the
one message pattern that dominates traffic. Consequence to be aware of: a client **alone** in a
room hears nothing at all, so after 30 s it hits `messageReconnectTimeout`
(`y-websocket.js:99,388-397`) and reconnects. That happens with the reference server too - its
ping is a WebSocket control frame, which does not reset the client's timer - and a reconnect
costs one `SyncStep1` and a diff. Rejected: sending traffic we do not need in order to keep a
client's timer happy.

### D29. Backpressure closes the connection at 1008
Per the brief. The outbound queue is 256 frames; when it is full the room closes that
connection with 1008 and drops it, without growing the buffer and without ever blocking the
room goroutine. Recovery is a reconnect and a diff against the client's state vector, which is
precisely what the CRDT makes cheap. Rejected: an unbounded queue (OOM under fanout), and
blocking the room (one slow client stalls everybody editing that document).

### D30. Removals do not bump the client's clock
Found by the 5-minute soak, not by reading the source. When a connection leaves, the server
retracts the awareness states it published. Bumping the clock on that retraction looks natural -
it guarantees peers accept it - but the departing client does not know we bumped it, so when it
reconnects and announces itself one clock past what it last sent, the announcement lands on an
equal clock and is rejected. The client stays a ghost until its own 15 s renewal pushes it past
us. Removals therefore go out at the client's own clock, which peers accept anyway under the
equal-clock null rule (`awareness.js:250`). `y-protocols` does the same: `removeAwarenessStates`
bumps only the *local* client's clock, never that of the peers it drops (`awareness.js:175-181`).

### D31. Room eviction calls a hook, and in Phase 2 the hook is nil
An idle room stops after five minutes and the document is dropped. `OnEvict` is the seam Phase 3
fills with a snapshot write. Rejected: introducing a `Store` interface with a no-op
implementation now, which would be a guess at Phase 3's shape dressed up as a design.

### D34. Room names map to document UUIDs by hashing
The schema keys documents by UUID; the wire protocol keys rooms by the URL path,
which people want to be readable. A name that already is a UUID is used as one; anything else
becomes a version 5 UUID under a fixed namespace. Deterministic, no lookup table, and the
schema stays exactly as the brief wrote it. Rejected: requiring UUIDs in the URL (unusable
demo, and it pushes name management onto every client), and adding a `name` column (a schema
change plus a second round trip on every open, to buy a rename feature nobody asked for).

### D35. Loading applies every remaining log row, not just those above snapshot_seq
The brief describes loading as the snapshot plus `doc_updates WHERE seq > snapshot_seq`. We
read the whole remaining log instead. `seq` comes from an identity column, and identity values
are handed out before commit, so a row with a lower seq can become visible *after* one with a
higher seq. With the brief's filter, such a row is skipped forever - silent data loss.
Compaction deletes exactly the rows it folded in, so anything still in the table has not been
folded in, and if a row is ever re-applied redundantly it costs nothing, because updates are
idempotent. This is the cheap half of the fix for [C6]; the expensive half is not needed yet.

### D36. The room encodes snapshots, the writer names the rows they cover
Compaction needs two things that live in different goroutines: the document, which only the
room may touch, and the set of log rows actually written, which only the writer knows. So the
room encodes the snapshot and hands it over, and the writer flushes what it has queued and
compacts against the seqs it has accumulated (see [D41]). The snapshot can cover more than
those rows do; the extra updates survive in the log and are applied again on the next load,
which is free. Rejected: having the writer ask the room for a snapshot (a round trip in the
other direction, and the room would still have to encode it), and letting the room decide which
rows are covered (it would have to guess, and a wrong guess deletes rows that were never
written).

### D37. Writes are asynchronous, and the durability window is the flush interval
The room hands updates to a per-room writer goroutine and carries on. A crash therefore loses
whatever was queued but not yet written - at most one flush interval, 200 ms by default. That
is acceptable here in a way it would not be in most systems, because the client that authored
the update still has it and pushes it again during the reconnect handshake; the acceptance test
covers exactly that path. Rejected: writing synchronously, which puts a database round trip in
the middle of every keystroke's fanout and makes the database decide how fast people can type.
When the write queue does fill, the room blocks rather than dropping: stalling is visible and
recoverable, and silently discarding updates that clients believe are saved is neither.

### D38. A failed read closes the room; a failed write does not
Asymmetric on purpose. If the document cannot be read, serving an empty one under that name
would let clients merge their state into a blank document and write the result back, which
destroys the document for everyone. So the room closes with 1011 and clients retry. If a write
fails, the document is still correct in memory and in every connected client, so the room keeps
serving and logs loudly. Losing durability is bad; losing the document is worse.

### D39. The resident-room cap evicts, it does not refuse
Now that a room can write itself out, hitting the cap should cost a snapshot, not a document:
the least recently used *idle* room is evicted to make space. A room with somebody connected is
never evicted - freeing memory by disconnecting people who are editing is not a trade the
registry may make - so the cap can still be reached, and then the join is refused.

### D42. ContentJSON is tested against hand-built bytes
Ref 2 is the one content type with no fixture: current `yjs` writes `ContentAny` for everything
the public API can produce, so the generator cannot emit one. Documents written by older
versions still contain it. The test therefore builds the update by hand from `ContentJSON.js`,
and the bytes were checked against real Yjs once: it reads the five values - including the
`undefined` that is not valid JSON - and re-encodes them byte for byte identically to what we
wrote. Rejected: leaving the branch untested because no fixture exists, which is how an
unreachable-looking decoder path turns into corruption the first time a real old document
arrives.

### D40. Pending deletions are advertised, unlike in Yjs
`EncodeStateAsUpdate` includes the deletions being held for structs we have not received, which
`Y.encodeStateAsUpdate` does not (it omits `pendingDs`). A peer that cannot resolve the range
holds it pending exactly as we do; a peer that can resolve it applies a deletion it was going to
receive anyway. The gain is that two replicas which have exchanged everything now agree, instead
of the deletion trailing its structs by one exchange, and that a pending deletion survives being
written into a snapshot. Rejected: matching Yjs exactly, which is the default position in this
project — but here the divergence is invisible on the wire (the delete set is a delete set
either way) and removes a real class of transient disagreement. Resolves [C5].

### D41. Compaction deletes rows by identity, not by range
`DELETE ... WHERE seq = ANY($folded)` rather than `seq <= watermark`. Identity values are handed
out before commit, so a range can take a row that the snapshot never saw. The room's writer
therefore accumulates the seqs it has written, hands them over with the snapshot, and clears
them once the transaction lands. Rejected: keeping the range and taking a heavier lock on the
append path, which would put contention on the hot path to protect an operation that runs once
per 500 updates. Resolves [C6].

### D33. The close frame goes out before the reader is unblocked
Found by running the gateway tests under `-race`, which turned an occasional flake into a
consistent failure: a connection the room closed with 1008 or 1002 arrived at the client as an
abrupt disconnect with no code. Cause: closing a connection cancelled the read context, and
`coder/websocket` tears a connection down immediately when its read context is cancelled - so
the write pump lost the race to send the close frame. The write pump now owns the whole
sequence: write the close frame, then cancel the reader, then signal the handler, which waits
before returning so the hijacked socket is not torn down underneath it. Without this the close
code is decorative, and the backpressure policy (D29) depends on the client being told why it
was disconnected.

### D32. The Go directive moved to 1.23
`github.com/coder/websocket` requires it. The brief asks for Go 1.22+, so this is inside the
constraint; noting it because it was a side effect of `go get`, not a decision made on purpose.

---

## Part 2 — The wire format, derived from source

### 2.1 lib0 primitives

| Primitive | Encoder | Decoder |
|---|---|---|
| `uint8` | `lib0/encoding.js:174` `writeUint8` | `lib0/decoding.js:146` `readUint8` |
| `varUint` | `lib0/encoding.js:260` | `lib0/decoding.js:245` |
| `varInt` | `lib0/encoding.js:277` | `lib0/decoding.js:277` |
| `varString` | `lib0/encoding.js:344` | `lib0/decoding.js:390` |
| `varUint8Array` | `lib0/encoding.js:434` | `lib0/decoding.js:122` |
| `any` | `lib0/encoding.js:544` `writeAny` | `lib0/decoding.js:501` `readAny` |

Bit constants (`lib0/binary.js:20,21,58,59`): `BIT7 = 0x40`, `BIT8 = 0x80`, `BITS6 = 0x3F`,
`BITS7 = 0x7F`.

**varUint** — plain LEB128-style base-128, little-endian groups, high bit = continuation:

```
while num > 127: emit(0x80 | (num & 0x7F)); num = floor(num / 128)
emit(num & 0x7F)
```

The decoder accumulates with multiplication (`num + (r & 0x7F) * mult`, `mult *= 128`), not
shifts, so values above 32 bits are supported up to `Number.MAX_SAFE_INTEGER` (2^53 − 1);
beyond that it throws (`decoding.js:258`). **Go: use `uint64` and return an error above
2^53 − 1**, so we fail the same way Yjs does instead of producing a value no browser can read.

**varInt** — this is the trap named in section 9. The first byte is laid out differently from
every following byte:

```
first byte:  C S V V V V V V     C = 0x80 continuation, S = 0x40 sign, V = 6 value bits
later bytes: C V V V V V V V     C = 0x80 continuation,               V = 7 value bits
```

From `encoding.js:277-291`:

```js
write((num > 63 ? 0x80 : 0) | (isNegative ? 0x40 : 0) | (63 & num)); num = floor(num / 64)
while (num > 0) { write((num > 127 ? 0x80 : 0) | (127 & num)); num = floor(num / 128) }
```

So the multiplier sequence on decode is 1, 64, 64·128, 64·128², … (`decoding.js:280`
`mult = 64`, then `mult *= 128`). The sign is **not** two's complement and **not** zigzag: the
magnitude is written unsigned and the sign lives in bit 6 of byte 0 only. Negative zero is
representable (`isNegative` comes from `lib0/math.js:60` `isNegativeZero`, true for `-0`), and
decodes to `-0`; Go should decode it as `0`.

**varString** = `varUint(byteLength of UTF-8)` followed by the UTF-8 bytes
(`decoding.js:377` decodes `readVarUint8Array` as UTF-8). Note the length is in **bytes**, not
code points.

**varUint8Array** = `varUint(len)` + raw bytes.

**any** (`writeAny`) — a one-byte type tag then the payload. Tags descend from 127 (the
comment table is at `encoding.js:516-531`); `readAny` indexes a lookup table with `127 - tag`
(`decoding.js:501`).

| Tag | Type | Payload |
|---|---|---|
| 127 | undefined | — |
| 126 | null | — |
| 125 | integer | `varInt` (only if integral and \|n\| ≤ 2^31−1) |
| 124 | float32 | 4 bytes, big-endian (`setFloat32(0, num, false)`) |
| 123 | float64 | 8 bytes, big-endian |
| 122 | bigint | 8 bytes, big-endian, signed |
| 121 | false | — |
| 120 | true | — |
| 119 | string | `varString` |
| 118 | object | `varUint(numKeys)` then `varString(key) any(value)` pairs, in `Object.keys` order |
| 117 | array | `varUint(len)` then `len` × `any` |
| 116 | Uint8Array | `varUint8Array` |

Floats are big-endian while every varint is little-endian — easy to get backwards.

### 2.2 Update v1 grammar

Produced by `writeStateAsUpdate` (`yjs/src/utils/encoding.js:504`) = `writeClientsStructs`
(`:81`) followed by `writeDeleteSet` (`yjs/src/utils/DeleteSet.js:222`).

```
Update      := StructsSection DeleteSet
StructsSection := varUint(numClients) ClientBlock*
ClientBlock := varUint(numStructs) varUint(client) varUint(startClock) Struct*
Struct      := info:uint8 StructBody
DeleteSet   := varUint(numClients) DsClientBlock*
DsClientBlock := varUint(client) varUint(numRanges) (varUint(clock) varUint(len))*
```

Ordering rules that must be reproduced for byte-identical output:

- Client blocks are sorted by client id **descending** (`encoding.js:99`
  `.sort((a, b) => b[0] - a[0])`, with the source comment "Write items with higher client ids
  first. This heavily improves the conflict algorithm"). Same for the delete set
  (`DeleteSet.js:227`) and for state vectors (`encoding.js:603`).
- Within a client block, structs are in ascending clock order, contiguous.
- `writeStructs` (`encoding.js:57`) writes `structs.length - startIndex` as the count, then the
  client, then the **clock the block starts at**, and writes the first struct with an
  offset — see 2.3.
- `writeClientsStructs` skips clients where the target already knows everything
  (`getState(store, client) > clock`), and treats clients absent from the target state vector
  as clock 0. This is exactly the `SyncStep2` / `DiffUpdate` computation.

### 2.3 The info byte and item body

`Item.write` (`yjs/src/structs/Item.js:655`):

```js
info = (content.getRef() & 0x1F)
     | (origin      === null ? 0 : 0x80)   // BIT8
     | (rightOrigin === null ? 0 : 0x40)   // BIT7
     | (parentSub   === null ? 0 : 0x20)   // BIT6
```

| Bits | Meaning |
|---|---|
| 0–4 (`0x1F`) | content ref (or `0` = GC, `10` = Skip — see 2.4) |
| 5 (`0x20`) | `parentSub` is non-null |
| 6 (`0x40`) | `rightOrigin` is present |
| 7 (`0x80`) | `origin` is present |

Body, in exactly this order (write side `Item.js:663-697`, read side
`encoding.js:144-164`):

1. if `info & 0x80`: origin — `varUint(client) varUint(clock)`
2. if `info & 0x40`: rightOrigin — `varUint(client) varUint(clock)`
3. if **neither** 0x80 nor 0x40 (`cantCopyParentInfo`): parent —
   `varUint(parentInfo)` where 1 means "root type" and is followed by `varString(key)`,
   and 0 means "child of an item" and is followed by `varUint(client) varUint(clock)`
4. if `cantCopyParentInfo` **and** `info & 0x20`: `varString(parentSub)`
5. content, per the ref table in 2.4

**Trap:** `parentSub` is flagged in the info byte whenever it is non-null, but is only written
when the item has neither origin nor rightOrigin. When it is not on the wire the reader
inherits parent and `parentSub` from the resolved left/right neighbour
(`Item.js:397-403`). A decoder that unconditionally reads a string when bit 5 is set will
desynchronise the byte stream on the very first map update with a left neighbour.

**Trap:** when a client block starts mid-struct, `writeStructs` passes
`offset = clock - firstStruct.id.clock` and `Item.write` substitutes
`origin = ID(this.id.client, this.id.clock + offset - 1)` (`Item.js:656`), i.e. a
self-referencing origin, and the content is written sliced (`ContentString.js:95`
`str.slice(offset)`, `ContentDeleted.js:83` `len - offset`, `ContentAny.js:88`). The encoder,
not the decoder, does the splitting.

### 2.4 Content refs

`readItemContent` dispatches on `info & 0x1F` through `contentRefs` (`Item.js:705,712`).

| Ref | Content | V1 encoding | Source |
|---|---|---|---|
| 0 | *GC struct* | `varUint(len)` | `GC.js:7,47` |
| 1 | Deleted | `varUint(len)` | `ContentDeleted.js:82` |
| 2 | JSON *(legacy)* | `varUint(len)` then `len` × `varString` (`JSON.stringify`, literal `"undefined"`) | `ContentJSON.js:83` |
| 3 | Binary | `varUint8Array` | `ContentBinary.js:76` |
| 4 | String | `varString` | `ContentString.js:94` |
| 5 | Embed | `varString` (`JSON.stringify`) | `ContentEmbed.js:79` |
| 6 | Format | `varString(key)` `varString(JSON value)` | `ContentFormat.js:87` |
| 7 | Type | `varUint(typeRef)` + type extras | `ContentType.js:153` |
| 8 | Any | `varUint(len)` then `len` × `any` | `ContentAny.js:86` |
| 9 | Doc | `varString(guid)` `any(opts)` | `ContentDoc.js:121` |
| 10 | *Skip struct* | `varUint(len)` | `Skip.js:8,45` |

Refs 0 and 10 are **struct kinds, not content**: they occupy the same 5 bits but the reader
branches on them before constructing an `Item` (`encoding.js:130-143`). `Skip` is written with
a plain `varUint` rather than `writeLen` — identical in v1, deliberately different in v2
(`Skip.js:47` comment).

Type refs for ref 7 (`ContentType.js:18-33`):

| typeRef | Type | Extra |
|---|---|---|
| 0 | `YArray` | — |
| 1 | `YMap` | — |
| 2 | `YText` | — |
| 3 | `YXmlElement` | `varString(nodeName)` (`YXmlElement.js:248`) |
| 4 | `YXmlFragment` | — |
| 5 | `YXmlHook` | `varString(hookName)` (`YXmlHook.js:84`) |
| 6 | `YXmlText` | — |

In V1 all of `writeLen`, `writeTypeRef`, `writeClient`, `writeParentInfo` are just `varUint`,
`writeString`/`writeKey` are `varString`, `writeInfo` is a raw `uint8`, `writeBuf` is
`varUint8Array`, `writeJSON` is `varString(JSON.stringify(v))`, `writeAny` is lib0 `any`
(`UpdateEncoder.js:36-125`; decoder mirror at `UpdateDecoder.js:33-119`). Note `writeJSON`
(refs 5, 6) and `writeAny` (ref 8) are *different* encodings — embeds and formats are JSON
strings, map values are lib0-any.

### 2.5 State vector

`writeStateVector` (`encoding.js:601`) / `readStateVector` (`:565`):

```
StateVector := varUint(numClients) (varUint(client) varUint(clock))*
```

`clock` is the **next expected** clock, i.e. one past the last integrated struct. Clients are
written in descending id order. An empty document encodes as the single byte `0x00`, which is
also the default `encodedTargetStateVector` in `encodeStateAsUpdateV2` (`:522`).

### 2.6 Delete set

`writeDeleteSet` (`DeleteSet.js:222`) / `readDeleteSet` (`:248`). Ranges are built by
`createDeleteSetFromStructStore` (`:188`), which merges adjacent deleted structs into one
`(clock, len)` range per run. In v1 `writeDsClock` / `writeDsLen` are plain `varUint`
(`UpdateEncoder.js:24,31`); v2 delta-encodes them against a running value — another reason to
implement v1 only.

Applying a delete set splits items at range boundaries (`readAndApplyDeleteSet`, `:278`) and
records deletes for clocks it has not seen yet in `pendingDs`.

### 2.7 Worked example

`testdata/fixtures/text-insert-single/state.bin`, 19 bytes — one client (1001) inserting
`"Hello"` into root type `text`:

```
01                    numClients = 1
01                    numStructs = 1
e9 07                 client = 1001          (0xe9 & 0x7f) | (0x07 << 7) = 105 + 896
00                    startClock = 0
04                    info: ref 4 (ContentString), no origin/rightOrigin/parentSub
01                    parentInfo = 1 -> root type follows
04 74 65 78 74        varString "text"
05 48 65 6c 6c 6f     varString "Hello"
00                    delete set: 0 clients
```

`sv.bin`, 4 bytes: `01 e9 07 05` — one client, 1001, next clock 5.

`msg-sync-step1.bin`, 7 bytes: `00 00 04 01 e9 07 05` — outer `messageSync`, then
`messageYjsSyncStep1`, then the state vector as a `varUint8Array`.

### 2.8 Sync protocol and framing

Two layers. The outer byte is defined by `y-websocket/src/y-websocket.js:20-23`:

| Outer type | Meaning |
|---|---|
| 0 | sync (payload is a `y-protocols/sync` message) |
| 1 | awareness (`varUint8Array` of an awareness update) |
| 2 | auth (`y-protocols/auth`) |
| 3 | query awareness (no payload) |

Inner sync messages (`y-protocols/sync.js:38-40`):

| Sub type | Payload |
|---|---|
| 0 `SyncStep1` | `varUint8Array(state vector)` (`sync.js:48`) |
| 1 `SyncStep2` | `varUint8Array(update)` — `Y.encodeStateAsUpdate(doc, theirSV)` (`sync.js:59`) |
| 2 `Update` | `varUint8Array(update)` (`sync.js:96`) |

Client behaviour on connect (`y-websocket.js:196-220`): send `sync/SyncStep1` immediately,
then, if it has local awareness state, an `awareness` message for its own client id. The
client replies to a received `SyncStep1` with a `SyncStep2` computed against the received
state vector (`readSyncMessage`, `sync.js:118` — note the reply is written into the same
encoder, so the reply's outer byte is written by the caller).

Server behaviour per the protocol comment (`sync.js:23-28`): on `SyncStep1`, reply with
`SyncStep2` **immediately followed by our own `SyncStep1`**; broadcast subsequent edits as
`Update`. On `queryAwareness` (outer 3), reply with an `awareness` message carrying every
known state (`y-websocket.js:53-67`).

Auth (`y-protocols/auth.js`): only `messagePermissionDenied = 0`, payload
`varString(reason)`. That is the wire shape Phase 5 must use to reject a bad JWT.

### 2.9 Awareness

`encodeAwarenessUpdate` (`y-protocols/awareness.js:194`):

```
AwarenessUpdate := varUint(numClients) (varUint(clientID) varUint(clock) varString(JSON state))*
```

`state` is `JSON.stringify(state)`, and `"null"` marks a client as gone. `applyAwarenessUpdate`
(`:241`) accepts an entry when `currClock < clock`, or when `currClock === clock && state ===
null` and the client is currently known — so removals are idempotent at equal clocks. Each
client increments its own clock on every state change (`:104`).

There is **no TTL on the wire**: `outdatedTimeout = 30000` (`awareness.js:13`) is a client-side
interval that drops peers whose state is older than 30 s and re-broadcasts the local state
every 15 s. Consequence for us, recorded as concern C3 below.

### 2.10 YATA integration

`Item.integrate` (`Item.js:419`) after `getMissing` (`:372`) has resolved dependencies.

1. If the struct arrives with an offset, split: `id.clock += offset`, left becomes the item
   ending just before it, `origin = left.lastId`, content spliced (`:420-426`).
2. `origin` resolves via `getItemCleanEnd` and `rightOrigin` via `getItemCleanStart` — both
   **split** existing items so that the neighbour boundaries are exact (`:385-392`).
3. Parent: taken from `left` if it is an item, else from `right`, else from the decoded parent
   (root key or parent item id) (`:395-411`).
4. Conflict resolution runs only when the neighbourhood changed, i.e.
   `(!left && (!right || right.left !== null)) || (left && left.right !== right)` (`:429`).
   Scan from `o = left.right`, or the leftmost item of `parent._map.get(parentSub)`, or
   `parent._start`, while `o !== null && o !== this.right`:
   - add `o` to `itemsBeforeOrigin` and `conflictingItems`;
   - if `origin === o.origin` (case 1): if `o.id.client < this.id.client`, move left past `o`
     and clear `conflictingItems`; else if `rightOrigin === o.rightOrigin`, **break**;
   - else if `o.origin !== null` and `itemsBeforeOrigin` contains `getItem(o.origin)`
     (case 2): if `conflictingItems` does not contain it, move left past `o` and clear;
   - else **break**.
5. Relink left/right, update `parent._start` or `parent._map`, and if `parentSub !== null` and
   there is no right neighbour, delete the previous value (`:507-516`) — that is how YMap
   overwrite works.

The tie-break in step 4 is the one section 9 warns about: **the item with the smaller client id
ends up to the left**, and it is only consulted when both items share an origin. Getting the
comparison backwards produces documents that agree most of the time and diverge under
concurrent same-position inserts — which is why `text-concurrent-same-index` and
`text-concurrent-after-shared-origin` are separate fixtures (the second one shares
*both* origin and rightOrigin, exercising the `break` branch).

### 2.11 Trap list

1. `varInt` first byte holds 6 value bits and a sign bit; every later byte holds 7. Not zigzag.
2. `parentSub` is on the wire only when both origin and rightOrigin are absent, regardless of
   the info bit.
3. Client blocks, delete-set clients and state-vector clients are all sorted **descending**.
4. Refs 0 (GC) and 10 (Skip) are struct kinds, not content types.
5. `writeJSON` (embed, format) is a JSON string; `writeAny` (map values) is lib0-any binary.
6. Floats and bigints are big-endian; varints are little-endian.
7. The first struct in a client block may be written sliced, with a synthesised
   self-referencing origin.
8. `varUint` is limited to 2^53 − 1 on the JS side; Go must not emit anything larger.
9. String struct lengths are **UTF-16 code units** (`ContentString.getLength()` returns
   `str.length`, `ContentString.js:22`), while the `varString` length prefix is **UTF-8
   bytes**. `"🎉"` is 2 units of clock and 4 bytes on the wire. Using `len(s)` or
   `utf8.RuneCountInString` for clock arithmetic diverges on the first emoji.

---

## Part 3 — Open concerns (brief rule 7)

### C1. Redis Pub/Sub is at-most-once, and nothing else heals a missed update
The architecture has no document ownership, which is right for CRDTs, but it makes fanout the
only path by which replica A learns about replica B's writes. Redis Pub/Sub drops messages on
subscriber disconnect or slow-consumer buffer overflow with no retransmit. A replica that
misses one message serves a permanently stale document to its clients: origin filtering does
not help, and the clients have no reason to reconnect.

Proposed fix at Phase 4, using machinery we already have: periodic per-room anti-entropy —
each replica publishes its state vector for active rooms every N seconds, and peers reply with
`Y.diffUpdate` against it. Same code path as `SyncStep1`/`SyncStep2`, bounded cost, self-healing.
Decision deferred to Phase 4; flagging now because it affects the fanout message schema.

### C5. A pending deletion is invisible to peers until its structs arrive — resolved
Found by the convergence property test, not by reading the source. If replica A receives a
delete for structs it has not seen, the deletion sits in `pendingDeletes`, and Yjs leaves its
`pendingDs` out of what it sends — so a peer syncing with A learns the structs but not that they
are deleted, and the deletion arrives one exchange behind them.

Resolved by including pending deletions in what we send ([D40]). A peer that cannot resolve the
range holds it pending exactly as we do, and a peer that can resolve it applies a deletion it
was going to receive anyway, so the change costs nothing and removes a case where two replicas
that have exchanged everything still disagree. It also stops a pending deletion being dropped
when a room writes its snapshot and restarts.

What remains is a different and more fundamental thing, now stated separately as [C7]: a diff
is computed against a state vector, so it cannot carry structs that were never integrated. That
is why the convergence property still exchanges to a fixpoint rather than asserting a fixed
number of rounds, and why Phase 4's anti-entropy has to keep running rather than fire once per
reconnect. Related to [C1].

### C7. A diff cannot carry structs a replica has not integrated
`EncodeStateAsUpdate` walks the struct store, so structs held in `pending` — those whose
dependencies have not arrived — are not in the diff at all. Two replicas each holding a
different link of the same chain therefore need one exchange per link before they agree. The
convergence property measures this directly: it exchanges until the two replicas stop changing,
with the number of updates as the bound, and 1000 random splits per scenario stay inside it.

Consequences: anti-entropy must run periodically rather than once (which [C1] needs anyway),
and no single round trip can be treated as "synced" for the purpose of, say, dropping a
document from memory.

The fix, if single-round convergence is ever wanted: encode pending structs too, bridging the
gaps with `Skip` structs (ref 10), which is exactly what `Y.mergeUpdates` does and which our
decoder already reads. Deliberately not done now — it is surgery on the Phase 1 core to remove
a round trip from a path that heals itself, and Yjs has the same behaviour, so no client is
surprised by it.

### C2. Compaction can race across replicas — resolved in Phase 3
Any replica may compact any document (section 3, no ownership). Two replicas compacting
concurrently could write snapshots out of order, and the loser's delete would then remove
updates the winning snapshot does not contain — silent data loss. Resolved as planned:
`Store.Compact` takes `pg_advisory_xact_lock` on the document and only writes a snapshot that
moves `snapshot_seq` forward, returning `ErrStaleSnapshot` and changing nothing otherwise. A
test asserts an older snapshot cannot overwrite a newer one. See also [C6], which is the part
of the same problem that is *not* fixed yet.

### C3. Awareness needs a server-side timeout — resolved in Phase 2
`outdatedTimeout` is client-side only (2.9). A client that vanishes without sending
`state: null` leaves a ghost cursor in every other client that has already synced its state,
because the server keeps rebroadcasting it on join. Resolved by `protocol.Awareness.Sweep`,
which the room runs every 5 s: a client that has been silent for 30 s is dropped and the
removal is broadcast. A connection that closes cleanly has its states retracted immediately
rather than 30 s later. The removal goes out at the client's own clock, not `clock + 1`, for
the reason in [D30].

### C6. Identity columns can commit out of order — resolved
`seq` is `GENERATED ALWAYS AS IDENTITY`, so values are handed out before commit and a row with
a lower seq can become visible after one with a higher seq. Two consequences, both now handled:

Loading reads the whole remaining log rather than `seq > snapshot_seq` ([D35]), so a
late-committing row is still applied.

Compaction deletes exactly the rows its snapshot folded in, by seq (`seq = ANY($1)`), instead
of everything at or below a watermark ([D41]). A range delete would take a row the snapshot
never saw. That mattered little on one node and would have mattered a great deal at Phase 4,
where two replicas append to the same document; fixing it while the code was still small cost
an array instead of an integer.

### C4. Section 8 vs. TipTap
Resolved in D8, kept here as a pointer: section 8's "no `YXmlFragment`" and Phase 2's "TipTap
works" are in tension, and the resolution is decode-everything / expose-little.
