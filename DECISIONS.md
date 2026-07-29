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

**`y-websocket@3.0.0` was added as a devDependency and needs your approval.** Reason: the
outer websocket framing (`messageSync = 0`, `messageAwareness = 1`, `messageAuth = 2`,
`messageQueryAwareness = 3`) is defined by `y-websocket`, not by `y-protocols`, and section 6
of the brief says derive the protocol from source rather than memory. It is also the client
library the Phase 2 demo will use. It is Node test tooling, not a Go dependency. Say the word
and I will drop it and hardcode the constants with a comment instead.

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

### D14. `-race` needs cgo, which needs a C compiler this machine does not have
`go test -race` on windows/amd64 requires `CGO_ENABLED=1` and gcc; `go env` reports
`CGO_ENABLED=0` and there is no gcc/clang on PATH. Tests currently run without `-race`.
This has no effect on `internal/crdt/lib0` (no goroutines), but it must be resolved before
the room actor and gateway land in Phase 2, where `-race` is the whole point. Options: install
a MinGW-w64 toolchain via winget, or run the race build in CI/WSL/Docker on Linux. Awaiting
your call.

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

### C2. Compaction can race across replicas
Any replica may compact any document (section 3, no ownership). Two replicas compacting
concurrently can write snapshots out of order, and the loser's `DELETE FROM doc_updates WHERE
seq <= snapshot_seq` can delete updates the winning snapshot does not contain — silent data
loss. Fix at Phase 3: guard the snapshot write with `WHERE snapshot_seq < $new` and take a
`pg_advisory_xact_lock(doc_id)` for the compaction transaction. Cheap, and it makes the "single
transaction" requirement in section 7 actually safe.

### C3. Awareness needs a server-side timeout
`outdatedTimeout` is client-side only (2.9). A client that vanishes without sending
`state: null` leaves a ghost cursor in every other client that has already synced its state,
because the server keeps rebroadcasting it on join. The server must track `lastUpdated` per
client id and broadcast a removal (state `null` at `clock + 1`) after 30 s of silence. Phase 2
detail, recorded here so it is not discovered in the demo.

### C4. Section 8 vs. TipTap
Resolved in D8, kept here as a pointer: section 8's "no `YXmlFragment`" and Phase 2's "TipTap
works" are in tension, and the resolution is decode-everything / expose-little.
