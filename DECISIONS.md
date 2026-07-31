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

### D43. The bus carries envelopes, not bare updates
Every message between replicas is `varUint(version) varUint(origin) varUint(kind)
varUint8Array(payload)`, encoded with our own lib0. The origin is what makes loop prevention
possible at all, the kind is what lets one channel carry updates, awareness and anti-entropy,
and the version is what makes a rolling restart legible instead of mysterious. Rejected:
publishing the raw update bytes to a channel per kind, which saves a few bytes and gives up the
origin - and without an origin, Pub/Sub delivering to the publisher means either a hop count or
a permanent echo. Rejected also: JSON, which would put a second serialisation format in a
project whose first one is tested byte for byte against real Yjs.

### D44. Loops are prevented by origin, at the subscriber
A replica drops any envelope carrying its own node id, and never publishes anything that came
off the bus. Together those two rules make a message cross the cluster exactly once, whatever
the cluster size, which is the property the Phase 4 acceptance criterion asks for a counter to
prove: with the clients quiet, update traffic goes to zero, and the counters say so on /statsz.
Rejected: a hop count or TTL, which bounds a loop instead of preventing it and needs tuning per
topology. Not available: not receiving your own publishes - Redis Pub/Sub has no such option.

### D45. The node id is random, not configured
64 bits from crypto/rand, masked to the 53 lib0's varUint can carry. Rejected: deriving it from
the hostname, pod name or replica index, all of which are one misconfiguration away from two
replicas sharing an id - at which point each ignores the other's traffic and the two diverge in
a way that looks exactly like a CRDT bug. Zero is reserved, so an unset field cannot be mistaken
for a valid id.

### D46. One multiplexed subscriber connection, not one per room
The Redis bus keeps a single subscriber connection and demultiplexes by channel name, adding
and removing channels as rooms come and go. Rejected: a subscription per room, which is simpler
to write and costs a Redis connection per resident document - a limit nobody wants to discover
during an incident, and one that would interact badly with the resident-room cap ([D39]).

### D47. Subscribing waits for Redis to confirm it
Found by the integration test, which failed immediately and consistently: `PubSub.Subscribe`
returns once the command has been written, so a publish issued straight afterwards can reach
Redis before the subscription exists and be delivered to nobody. For a room that is precisely
the case that matters - it subscribes, then serves a client whose first edit must not vanish -
so the bus now waits for the server's subscription confirmation, which arrives on the receive
loop as a `*redis.Subscription`. That is also why the loop calls `Receive` rather than
`ReceiveMessage`: the latter swallows exactly the message being waited for. Rejected: sleeping.
Rejected: accepting the loss on the grounds that anti-entropy would repair it, which trades a
correctness property for the absence of ten lines.

### D48. An update is written by the replica whose client authored it
Remote updates are applied and relayed to local clients but not appended to the log. Every
update is therefore in the log exactly once, from its origin. Rejected: having every replica
write everything, which multiplies the write rate and the log size by the replica count to
protect against a case - the origin dying inside its flush window - that the client's own
reconnect already covers ([D37]). This is also the point where [D41] stops being a nicety: with
two replicas appending to one document, a compaction that deleted a *range* would delete rows
the other replica had written and this one had never seen.

### D49. Anti-entropy is a periodic state vector, answered with a diff
A room with connections publishes its state vector every 15 s; a replica that sees one computes
`EncodeDiff` against it and publishes the difference, or nothing at all when there is none.
This is the `SyncStep1`/`SyncStep2` exchange the clients already perform, run between replicas
on a timer, and it is what makes an at-most-once bus safe to build on. Resolves [C1].
Rejected: reconciling once when a room is created, which fixes a restarting replica and not a
dropped message, and which [C7] rules out anyway - one exchange is not a fixed point. Rejected:
Redis Streams, which would give at-least-once delivery and take back consumer groups, trimming
and a second failure mode; the periodic exchange costs one short message per room per replica
and repairs everything, including whatever a Streams consumer would still have missed.

### D50. A timeout is local; a disconnect is published
When a connection leaves, its awareness retraction goes out on the bus, so the cursor
disappears everywhere at once. When the awareness sweep times a client out, it does not: a
timeout is this replica's conclusion drawn from its own silence, and the replica the client is
actually connected to is the one that knows better. Publishing sweeps would let a node with a
slow bus connection delete live cursors across the cluster.

### D51. Publishing is off the room goroutine, and drops rather than blocks
The room hands envelopes to a bounded queue drained by a publisher goroutine, and the bus hands
envelopes to a bounded per-room queue drained by the room. Neither side ever blocks the other:
a document must not wait for the network, and one busy document must not stall every other
document on the node. Both queues drop and count when full, which is honest - the next
anti-entropy round repairs the loss - and both counters are on /statsz, so a node under that
much pressure is not silent about it. Rejected: blocking, which is what the *persistence* queue
does ([D36]) and is right there, because losing a write is unrecoverable while losing a fanout
message is not.

### D52. A room that cannot join the bus refuses to serve
If the subscription fails, the room closes with 1011 instead of starting. Rejected: serving
anyway, which produces the worst failure this system has - a document that looks fine, accepts
edits, and silently ignores everyone on the other replicas. Failing makes the client reconnect,
probably to a replica that is healthy.

### D53. /statsz is JSON, and deliberately small
The cluster counters are exposed as a flat JSON object. Prometheus is Phase 6 and this is not a
substitute for it: it exists because the Phase 4 criterion is about numbers a test has to read,
and a test that has to parse an exposition format to learn whether updates looped is a test
nobody writes.

### D54. Caddy does not pin a client to a replica
No sticky sessions, no affinity. The point of this phase is that any replica can serve any
document, so a load balancer that pinned clients would hide the very thing the deployment
exists to demonstrate. A client that reconnects lands wherever it lands and resyncs from its
state vector, which costs one diff.

### D55. A token names its document, so it is a capability rather than a login
The `doc` claim is required and must match the document being opened. A token
that leaks therefore opens one document until it expires, instead of being a key
to the server. Rejected: authenticating the user and looking their permissions up
in a table, which is what most systems do and which would put a database read on
the connection path and a permission model in a server that has no idea what a
user is. The application that knows who its users are mints these tokens; this
server only checks them.

### D56. HS256 with a shared secret, and the algorithm is pinned
One symmetric key, given to the server and to whatever mints tokens. Rejected:
RS256 or a JWKS endpoint, both of which are better when the issuer is a separate
system that already exists, and both of which add key distribution or a network
call to a phase whose job is to make the seam real. The parser is pinned to
HS256 with `jwt.WithValidMethods`: without that, a token whose header says
`"alg":"none"` is accepted, which is the oldest bug in JWT and is why the test
suite mints one and checks that it is refused.

### D57. Several secrets may be configured at once
`-jwt-secret a,b` accepts tokens signed with either. That is the whole of key
rotation: add the new key, wait for the tokens signed with the old one to expire,
remove the old one. Rejected: a single key, which makes rotation an outage.

### D58. Expiry is required by default, and may be capped
A capability with no expiry is a permanent one, and the safety of putting a token
in a URL rests entirely on it being short-lived - a URL ends up in browser
history, in access logs and in whatever the user pastes into chat. So a token
with no `exp` is refused unless `-jwt-require-expiry=false`, and
`-jwt-max-lifetime` refuses tokens valid for longer than a deployment wants to
allow. Rejected: trusting the issuer to be sensible, which is a policy the server
can enforce for the cost of two comparisons.

### D59. The token travels in the query string
`?token=...`, which is what `y-websocket`'s `params` option writes. Rejected: an
`Authorization` header, which a browser cannot set on a WebSocket - the API
simply has no argument for it - so requiring one would mean writing a custom
client and giving up the project's central promise that unmodified Yjs clients
work. Rejected also: inventing an auth frame the client sends after connecting,
which would be a message `y-websocket` does not send, breaking the same promise.
A header *is* accepted as an alternative, for clients that are not browsers. The
cost is that the token appears in URLs, which is exactly what [D58] is about.

### D60. Authorisation happens before the upgrade, and the refusal is sent after it
The check runs before `websocket.Accept`, so a request that fails never reaches
a room and cannot even cause one to be created. The *rejection*, though, is
written over the upgraded connection as a `y-protocols/auth` permission-denied
message followed by a 1008 close, because that is where the reference client
reads it (`y-websocket.js:84-92`): a client refused with an HTTP status simply
retries forever, while one that receives this message stops. Rejected: replying
401 before upgrading, which is more correct as HTTP and worse as behaviour.

### D61. Read-only is enforced in the room, not at the edge
The gateway decides the permission once and the room applies it, because the room
is the thing that integrates updates - and after Phase 4 it is also the thing that
publishes them to other replicas. Enforcing it there means a refused update is
never applied, never persisted, never relayed to a local peer and never put on
the bus, all from one check.

An empty update is exempt. A well-behaved read-only client answers our SyncStep1
with a diff, and once it has the document that diff carries nothing; treating it
as an attempt to write would disconnect every read-only client on its second
message.

### D62. A read-only client that writes is told, and disconnected
It gets a permission-denied message and a 1008 close. Rejected: silently
discarding the update, which is tempting because it keeps the connection alive -
and which shows the user a document that looks edited, will not survive a reload,
and diverges from what everyone else sees. A visible failure is kinder than a
document that lies. This is the same policy as backpressure ([D29]): say why,
then close.

### D63. The write pump flushes its queue before the close frame
Refusing an update means queueing an explanation and then closing the connection,
and the write pump selected between "a frame is waiting" and "we are closing"
with no order between them - so the explanation was lost about half the time.
The pump now drains whatever is queued, then writes the close frame, then
unblocks the reader ([D33]). The queue is bounded at 256 and every write has its
own deadline, so a hostile peer cannot hold the shutdown open.

### D64. Without a secret, the server is open, and says so
No `-jwt-secret` means every connection may read and write, and the server logs a
warning at startup, alongside the ones for no database and no Redis. Rejected:
refusing to start without one, which would make the demo a configuration exercise
and push people towards checking in a secret. Rejected also: staying quiet, which
is how an open server reaches production.

### D65. The operator endpoints live on their own listener
`/metrics`, `/statsz` and `/debug/pprof` are served on `-admin-addr`, which
defaults to `127.0.0.1:6060`, and none of them is on the port clients connect to.
pprof will dump the heap, print the command line the process was started with,
and block the process for a thirty-second CPU profile on request: on a public
port that is both an information leak and a way for anyone to stall the server.
A separate listener lets the deployment decide who can reach them - a bind
address, a firewall rule, or in Kubernetes simply not naming the port in the
Service. `-pprof=false` turns the profiler off while leaving the metrics, for a
deployment that cannot isolate the port. Rejected: serving them on the main port
behind a path prefix, which is one misconfigured proxy away from being public.
`/healthz` stays on both, because a load balancer probes the port it sends
traffic to.

### D66. Metrics are a struct that is passed in, not package-level variables
`metrics.New(registry)` returns every collector, and the packages that report
into it take one in their config. That costs two config fields and buys two
things: a test builds a fresh set against its own registry and asserts on what
was recorded, and `metrics.Nop()` exists - registered nowhere - so no call site
ever checks for nil before counting something. Rejected: package-level
collectors on the default registry, which is what most Go code does and which
makes two tests in one binary collide.

### D67. Labels are a fixed small set, never a document name
Close codes are labelled, message types are labelled, refusal reasons are
labelled with a slug rather than the error text. No metric carries a room name
or a client id. A label whose values come from user input is how a metrics
endpoint becomes the thing that takes the server down: cardinality is memory
here and in whatever scrapes it.

### D68. The load bot speaks the wire protocol, not Yjs
`cmd/ycollab-load` builds updates by hand and holds a socket per client, so a
thousand clients cost a thousand goroutines and a few megabytes. Rejected:
driving real `y-websocket` clients, which is what `tools/soak` does and is the
right tool for *correctness* - but a real client costs a document, a provider and
a couple of megabytes, so a few hundred of them make the load generator the
bottleneck and measure nothing about the server. Wire correctness is already
covered byte for byte by the fixtures, so the bot is free to be cheap.

Its update builder is the one place outside `internal/crdt` that writes Yjs
structs, which is a real risk of drift, so it self-tests at startup: it builds a
chain, integrates it with the actual engine and checks the text. A malformed
builder fails there instead of as a stream of 1002 closes.

### D69. The bot connects in waves
Dialling a thousand sockets at once overran the listen backlog and the kernel
refused a fifth of them - which measures the accept queue, not the server, and is
not what any real population of clients does. `-connect-at-once` bounds the dials
in flight.

### D70. The latency report states the clock's resolution
The propagation percentiles are printed next to the smallest step this machine's
monotonic clock can take, measured at startup. On the development machine that
step is about half a millisecond, so a p50 below it is reported as `0s` - which
without the resolution line reads either as a bug or as a boast, and is neither.
Rejected: printing microseconds the clock cannot resolve.

### D71. A removed awareness entry is forgotten after ten minutes
Removing a cursor keeps its clock, so a replayed update cannot resurrect it. That
was right and incomplete: a Yjs client picks a new random id for every `Y.Doc`,
so every reconnect left an entry behind, and a room that stays resident for days
grew one per reconnect. It was a slow leak in exactly the rooms that matter most,
found by reading the code rather than by any test failing.

The sweep now drops entries whose state was removed more than ten minutes ago -
far past any in-flight duplicate, far short of a working day. The worst case if
a genuinely ancient update arrives afterwards is one ghost cursor, which the next
sweep removes. Rejected: keeping them forever, which is correct and unbounded;
rejected: dropping them immediately on removal, which would let a duplicate
removal or a slow peer resurrect a cursor.

### D72. A node caps its connections
`-max-conns` refuses connections past a limit with 503, before the upgrade. A
connection costs two goroutines, a 256-frame queue and a room, and nothing else
in the server bounded how many there could be: with `-jwt-secret` unset there was
no gate at all, and with it set there was still none on how many connections one
tokenholder could open. Default is zero, meaning no cap, which is right for a
laptop and wrong for anything reachable - so the Kubernetes manifest sets it, as
it sets `-max-rooms`.

### D73. Durable writes are an option, not the default
`-durable-writes` makes the room wait for an update to be on disk before it
relays it, closing the flush window [D37] leaves open. It is off by default
because it puts a database round trip on every keystroke, and the window it
closes is already covered for the case that actually happens: a client that was
editing when the server died still holds its own copy and sends it back on
reconnect. It exists because "how much may we lose" is a question only the
deployment can answer, and the honest answer for some of them is "nothing".

### D74. Documents can be deleted, and retention is opt-in
`DELETE /documents/{name}` on the admin listener removes a document from memory
and from the database; the log goes with it through the foreign key's cascade. A
document somebody is editing is refused with 409 rather than deleted out from
under them.

The admin listener is the authorisation. A token would be the wrong mechanism:
the tokens this server understands are per-document capabilities minted for
editors, not operator credentials, and inventing a second kind would be inventing
an identity system.

`-retention` deletes documents nothing has touched for a given period, off unless
asked for. A collaborative document nobody has opened in months is still
somebody's document, and deleting it by default would be the server making a
policy call it has no standing to make. Activity is read from the log rather than
maintained as a column, which keeps the append path at one statement: appending
is the hot path, retention runs a few times a day.

### D75. The cluster counters are collected at scrape time
`/metrics` exposes the same numbers as `/statsz` through a collector that reads
the room manager's atomics when Prometheus asks. Rejected: incrementing a
Prometheus counter next to each atomic, which is the obvious implementation and
gives two sources of truth for one fact - the kind of duplication that ends with
a dashboard and an endpoint disagreeing during an incident.

### D76. Measured before optimised
`internal/crdt` has benchmarks for the three paths a busy server lives in. On the
development machine: integrating an update is about 630 ns, an update that is
already known costs 415 ns to reject, a state vector encodes in 480 ns, and a
20 KB document produces a full diff in 23 µs. The load runs put the server at
98,000 delivered frames a second with nothing dropped, and the server's own
histogram puts the mean integration at about a microsecond.

So no optimisation was applied: at those numbers the engine is not what limits
this server, and a speculative change to the one part of the codebase whose
output is verified byte for byte would be trading a real guarantee for an
imaginary gain. The benchmarks are committed as the baseline the next person
argues against.

### D77. An awareness state is bounded, and so is the number of them
A cursor is a name, a colour and a couple of offsets - a few hundred bytes. The
frame limit is 16 MiB, so until now one client could publish a 16 MiB "cursor",
which this server held in memory, relayed to every peer in the room and published
to every replica. That was the cheapest amplification the server offered, and it
needed no permission beyond being allowed to connect. Found while writing the
project evaluation, not by a test.

`-awareness-max-state` (4 KiB) and `-awareness-max-clients` (1024) bound both
dimensions: the size of a state and how many clients one document tracks. Client
ids are chosen by the client, so without the second one connection could invent
millions of them.

Three details that took a second pass:

  - A removal is always accepted, whatever the limits say. Refusing one would
    leave a cursor on screen that its owner had retracted.
  - The cap counts *cursors*, not entries. A removed entry is a remembered clock
    ([D71]), and holding a slot for it would make a full room refuse newcomers
    for ten minutes after every departure - which the first version did, and a
    test caught.
  - Remembered clocks are themselves capped at twice the limit, oldest dropped
    first, because a client cycling through ids would otherwise grow the map for
    the whole ten-minute window.

Rejected: refusing the offending entry and continuing. The room already closes a
connection that sends an unusable awareness update, and a client publishing a
megabyte of cursor is one this server should not be carrying for.

### D78. A refused cursor closes the local connection and only counts a remote one
A local connection gets a permission-denied message and 1008, the same treatment
backpressure and read-only writes get: say which rule was broken, then close. An
oversized state that arrived over the cluster bus is counted and dropped instead.
There is nobody to disconnect there, and taking a replica down because a client
on the far side of the cluster misbehaved would spread the problem rather than
contain it.

### D79. Rate limiting slows a connection, it does not close it
A token bucket per connection, on two dimensions: messages per second and bytes
per second. They are two different floods - many small updates cost CPU per
message, a few large ones cost memory and fanout bandwidth - and a limit on one
alone leaves the other open.

Over the limit the read pump waits. It does not close the connection, because a
client that bursts is usually a client being used, and disconnecting it costs a
reconnect and a resync to fix something that resolves itself in a millisecond.
Waiting also puts the backpressure where it belongs: we stop reading, the socket
buffer fills, and the sender's own TCP stack slows it down. Only that connection
is affected - the limiter is per connection and the room never waits on it.

Defaults are on and generous: 200 messages a second and 8 MiB a second, against
the ten to thirty messages a second a person actually produces. They exist to
stop a client that has gone wrong, not to shape normal traffic. Zero means the
default and negative means no limit, so "unset" and "unlimited" are not the same
word.

Verified against a real server: ten clients pushing 60 messages a second each
into a 20/s limit were slowed to the limit with zero errors and zero
disconnections, and the throttle counters recorded 2,919 waits totalling 144
seconds.

### D80. 1011 is for a document we could not serve, not for a peer that left
That load run showed ten connections closing with 1011, "internal error",
because a write that failed after the client went away was being recorded that
way. The close code never reaches a peer that is already gone; what it reaches is
the metric, and "ten internal errors" every time ten people close a tab is how an
operator learns to ignore the graph. A failed write is now 1001. 1011 is left to
mean what it says: this server could not serve this document.

### D81. A hook is an observer, never a participant
Hocuspocus lets a hook be awaited, and so refuse or delay what the client did.
This one cannot. A document that stops accepting keystrokes because somebody's
webhook receiver is slow is a worse outcome than a missed notification, every
time, and the failure is remote, intermittent and invisible from the editor.

So `Emit` never blocks, the queue is bounded, and a full queue drops events and
counts them. Authorisation already has a place to live - the token, checked
before the connection is accepted ([D60]) - so the thing a blocking hook would
be for is answered somewhere better.

Rejected: an optional blocking mode for the deployments that want it. The knob
would be off in every test and on in production, which is the arrangement that
guarantees the failure is first seen by a user.

### D82. Events are coalesced onto the room's tick, not sent per update
A person typing produces tens of updates a second. A webhook per update is an
outage at the receiving end and a queue that is permanently full at this one.

The room therefore marks that something changed and emits at most one event of
each kind per tick. That costs an increment on the hot path, needs no second
timer per document, and produces an event that says "twelve updates since the
last one" rather than twelve events. A room with no hooks configured does none
of it, including the state-vector encode.

The consequence to know about: the delay between an edit and its webhook is
bounded by `-tick` (5 s by default), not by a knob of its own. Rejected: a
per-document debounce timer, which is a second timer per resident document for a
feature most deployments do not turn on, to buy latency that `-tick` already
buys.

### D83. Only local edits raise a change event
Every replica holding a document applies every edit, so the naive version turns
one keystroke into one webhook per replica. The event is raised where the client
is: an update that arrived on the cluster bus is applied and relayed but not
reported, because the replica whose client made it already reported it.

`document.store` is different and deliberately not deduplicated - it is raised
by whichever replica actually wrote rows, which is a fact about that replica.

A receiver still has to be idempotent. Two people editing the same document on
two replicas produce an event from each, both true, and a retried delivery
repeats one - which is why the delivery id is generated once per event and
reused across its retries, so a receiver can recognise the repeat.

### D84. The signature covers the timestamp, and a store event means the write happened
Two details that are the difference between a webhook and a webhook somebody can
build on.

The signature is `HMAC-SHA256(secret, "<unix>.<body>")`, sent as
`t=<unix>,v1=<hex>`. The timestamp is inside the signed text rather than beside
it, so a captured request cannot be replayed later with a fresh timestamp:
changing it breaks the signature. `hook.Verify` is exported and is what the
tests use, so the format has exactly one reader and one writer.

`document.store` is raised by the persist goroutine after `Append` returns
without an error, not by the room when it queues the write. The room learns
about it through an atomic counter it drains on its next tick. A failed write
raises nothing, which is the point: a receiver mirroring the document must not
record that it was saved when it was not.

### D85. A read is served by the room when there is one, and never starts one
`GET /documents/{name}` asks the resident room through its mailbox, like every
other command, so what it returns is exactly what the connected clients are
looking at - up to a flush interval ahead of the database. A document with no
room is read from the store directly.

Rejected: starting a room for the read, which is the obvious implementation and
the wrong one. It would hold the document in memory, join it to the cluster,
begin its idle timer and start it emitting hooks, all as a side effect of
somebody looking at it. A read should not change what the server is doing.

The cost is stated rather than hidden: in a cluster, an edit still sitting in
another replica's flush window is not visible to a read here. Making it exact
would mean a bus round trip on every read, on the chance that some other node
holds the document.

### D86. The state vector is the ETag
It is already the version - it is what a client sends to say what it has, and
what anti-entropy compares - so `If-None-Match` and `?sv=` fall out of it rather
than being invented. Two replicas holding the same document produce the same
ETag, which is what makes it usable behind a load balancer, and `?sv=` turns the
state vector a webhook just delivered into a diff.

Rejected: a monotonic version counter. It would have to be agreed across
replicas, which is a consensus problem this server does not otherwise have, to
replace something the protocol already carries.

### D87. The JSON view has no "type" field, because the wire format has none
The first version reported a type per root and a `rendered` flag. A test found
every root coming back as `unknown`, and the reason is in `doc.go`: **the v1
format never states a root type's kind.** `doc.getText('x')` and
`doc.getMap('x')` read the same bytes two ways, and Yjs decides at the moment
the client asks. A server that only ever saw updates cannot know which was
meant.

So the field is gone rather than guessed. Each root offers both readings - `text`
for the sequence, `keys` for the map - and one of them is empty. A guess would
have been right most of the time, which is worse than useless: it would be
believed.

The binary form remains the complete one. The JSON view is a convenience, and
for an XML root it shows a name and no content, because the engine decodes XML
and exposes no reader for it ([C4], [D8]).

### D88. Reading lives on the admin listener, not on the client port
Two reasons, and the second is the one that decides it.

The document name is the URL path on the client listener, so `ws://host/notes`
is document "notes". Any HTTP route added there is a document name somebody can
no longer use.

And there is no token that would authorise it. The tokens this server
understands are per-document capabilities minted for an editor ([D60]); "read
any document" is an operator power, and the admin listener is where operator
powers already live, next to `DELETE /documents/{name}`, with the same argument:
the deployment decides who can reach the port.

### D89. Restoring is POST and it merges, because the format cannot replace
A backup nobody can restore is not a backup, so `GET /documents/{name}` needed a
counterpart. It is POST rather than PUT, and the difference is not pedantry:
these are CRDT updates. Applying one adds what it carries to what is already
there, and the v1 format has no operation that makes a document forget - a
deletion is itself an update saying what was deleted. A PUT would promise a
replacement this server cannot perform.

So the method, the handler's name and the documentation all say *merge*, and the
runbook says to `DELETE` first when you want the backup rather than the union of
the backup and whatever the document has become.

A merge into a resident room goes through the mailbox like every other command,
is broadcast to every connection and published to the cluster. Clients that were
already connected have to hear about it, or they keep building on a version the
server no longer holds.

Rejected: refusing to merge into a document that has connections, the way DELETE
does. DELETE is destructive and a merge is not; refusing would mean a document
can only be restored when nobody is looking at it, which is the opposite of when
somebody notices it needs restoring.

### D90. A restore with no local room still publishes to the bus
When no room on this node holds the document, the update is appended to the log
directly. That is enough for the next load - and invisible to a replica that has
the document resident right now, which would not reread it until it evicted, and
would then write its own snapshot over the restore.

So `Import` publishes the update on the cluster bus as well, under this node's
id. Any replica holding the document applies it as an ordinary remote update.
There is no room here to filter it back, and there is nothing to loop.

Rejected: telling the operator to stop the cluster first. It is correct advice
for a whole-database restore, where the tables move underneath every replica at
once, and the runbook gives it there. For one document it would be a footgun
disguised as a procedure.

### D91. The runbook's commands were run, not written
Every command in `docs/RUNBOOK.md` was executed against the real containers
before it went in, and the backup section is a transcript. The one that mattered
was `pg_restore`: `doc_updates.seq` is `GENERATED ALWAYS AS IDENTITY` and the
primary key is `(doc_id, seq)`, so a restore that reset the sequence would make
the next write to every restored document collide. It does not - `max(seq)` and
the sequence's `last_value` were both 76 after the round trip, and the next
insert got 77 - but that is a fact worth checking rather than assuming, and the
runbook tells the reader to check it too.

The per-document backup and restore is not documented on trust either: it is
what `TestADocumentCanBeBackedUpAndRestored` performs on every CI run.

### D92. The admin listener gets a token, because it stopped being read-only
The original argument for no authentication was that the deployment decides who
can reach the port ([D65]), and it was sound while the surface was metrics,
stats and pprof. Then it grew `DELETE`, and then `POST` ([D89]), and now a
request that reaches it can rewrite any document on the server. Network
isolation is still the first control, but it became the *only* control on a
destructive surface, and one misconfigured Service or one forwarded port is the
whole distance between "internal" and "anybody". Found while scoring the
project, not by an incident.

`-admin-token` is a shared secret checked as a bearer credential, deliberately
not the JWT machinery: those tokens are per-document capabilities minted for
editors ([D60]), and "may administer this server" is not a document capability.
A shared secret is the right shape for a surface whose users are an operator, a
scrape job and a backup script.

Three details:

  - **Everything except `/healthz`.** A load balancer probing liveness cannot
    hold an operator credential, and the answer is one word that says nothing
    about the documents. It is registered on an outer mux rather than excepted
    inside the check, so "what is open" is one line to read instead of a
    condition to reason about. `/metrics` is *not* excepted - Prometheus can
    send a header, and pprof next door prints the command line the process was
    started with.
  - **Header only, never a query parameter.** A token in a URL ends up in
    access logs, browser history and the `Referer` of anything the response
    links to. The WebSocket endpoint accepts `?token=` because a browser cannot
    set a header on a WebSocket handshake; nothing on this listener has that
    excuse.
  - **Compared as SHA-256 digests.** `subtle.ConstantTimeCompare` returns early
    on a length mismatch, so comparing the raw strings would leak the secret's
    length. Digesting first makes every comparison fixed-width.

Rejected: refusing to start without a token. A laptop running the demo has
nothing to protect and the listener defaults to localhost, so it is a warning at
startup, the same treatment running without a signing secret gets ([D64]).

### D93. X-Forwarded-For is ignored unless a proxy is named
Nothing in this server read a peer address at all, so an incident had no answer
to "where is it coming from" and `-max-conns` was a limit one client could reach
alone.

The address has to be established carefully, because the header that usually
carries it is written by the client: `X-Forwarded-For` is a request header like
any other, and anyone can send one saying anything. Trusting it unconditionally
would let an attacker pick their own identity - and a per-address limit an
attacker can opt out of is *worse* than none, because it costs the work and
gives the false impression of a control.

So the header is read only when the machine on the other end of the socket is a
proxy the operator named in `-trusted-proxies`, and the address taken is the
**rightmost** entry that is not itself trusted: the last address a machine we
trust actually saw. Taking the leftmost is the classic mistake - that end of the
list is the part the client wrote. A hop that will not parse stops the walk,
because the chain cannot be reasoned about past it.

`loopback` and `private` are named shorthands for the two real deployments,
because the alternative is every operator writing out the same RFC 1918 blocks
and one of them getting it wrong. A malformed entry is an error at startup, not
a silently narrower set.

The address goes in the logs and never in a metric label: a label taken from a
peer address makes the endpoint's cardinality the number of addresses on the
internet ([D67]).

### D94. The per-address cap is off by default, unlike the rate limits
The rate limits ship on ([D79]) on the argument that a limit which only catches
a client that has gone wrong should not need configuring. The same argument does
not carry here, and the difference is NAT.

A rate limit is per connection, so its false-positive case is one misbehaving
client. A per-address cap is per *address*, and an office, a school or a mobile
carrier is one address to this server - a live default would be a live default
on how many colleagues may edit at once, breaking exactly the collaborative case
this server exists for. The deployment knows its clients; the Kubernetes
manifests set it to 100.

There is a matching footgun the server warns about: behind a load balancer with
no `-trusted-proxies`, every client looks like the load balancer, so the cap
would apply to the whole deployment at once.

### D95. A version is a whole document, not a diff
The obvious design is a chain of diffs, and it would be smaller. It was rejected
for two reasons that both bite at the moment somebody needs it: reading one
version becomes a walk through every version before it, and there is no way to
drop an old one - which is the only operation retention consists of. A history
you cannot prune is not a history, it is a leak.

So each row is a complete Yjs update, and restoring is one read. What keeps the
size honest is that a version is written only when its state vector differs from
the newest one, so a document nobody edited produces one row however long the
timer runs. `-version-keep` (24) bounds the rest.

Rejected also: Yjs's own `Y.snapshot`, which is a state vector plus a delete set
and reconstructs a past state with `createDocFromSnapshot`. It only works on a
document created with garbage collection off, and it would need that function
implemented in this engine - a large piece of work whose output would be the
same bytes this design stores directly.

### D96. The duplicate check is a condition on the insert, not a read then a write
Every replica holding a document runs its own version timer, so the naive
implementation produces one version per replica per interval.

`SaveVersion` is a single statement whose `WHERE NOT EXISTS` covers both the age
gate and the state-vector check. There is no window between deciding and
writing, so three replicas offering a version in the same second produce one row
- whichever gets there first - with no lock, no leader and no coordination.

The room keeps a `versionDirty` flag purely as an optimisation: an unchanged
document does not encode itself every interval only to have the store refuse it.
Correctness lives in the statement; the flag saves the 26 µs.

### D97. Restoring is DELETE then POST, and there is no restore endpoint
`POST /documents/{name}` merges ([D89]), and CRDT updates cannot remove what a
document has since gained. A `POST /versions/{id}/restore` would therefore hand
back the union of the old version and the damage somebody was trying to undo -
an endpoint whose name promises more than the format can deliver.

Two visible steps say what is actually happening. The runbook and the README
both spell them out, and `TestAVersionSurvivesSomebodyWreckingTheDocument` runs
them end to end.

### D98. Restoring into a document that never existed had to be made to work
Writing that test found a real defect. `doc_updates` has a foreign key to
`documents`, and the only thing that created a document row was `Load` - as a
side effect of reading. So `Import` appended to the log of a parent that did not
exist and returned 500.

The backup test written a session earlier passed anyway, because it happened to
`GET` the document between deleting and restoring it, and that read created the
row. The bug was invisible until a test skipped the read - and the case it broke
is restoring into an empty database, which is the disaster-recovery path.

`Store.Ensure` is now an explicit statement on the `Persistence` interface, and
`Import` calls it. The regression has its own test rather than staying covered
by accident.

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

### C1. Redis Pub/Sub is at-most-once, and nothing else heals a missed update - resolved
The architecture has no document ownership, which is right for CRDTs, but it makes fanout the
only path by which replica A learns about replica B's writes. Redis Pub/Sub drops messages on
subscriber disconnect or slow-consumer buffer overflow with no retransmit. A replica that
misses one message would serve a permanently stale document to its clients: origin filtering
does not help, and the clients have no reason to reconnect.

Resolved by periodic anti-entropy ([D49]): a room with connections announces its state vector
every 15 s, and any replica holding more answers with the difference. Convergence is therefore
eventual with a bounded delay rather than immediate, which is the honest guarantee an
at-most-once bus can support. The integration test drives the loss directly - it edits a
document on one replica while a second replica has no room for that document at all - and the
second replica catches up with no database involved.

This project's own drop paths ([D51]) rely on the same mechanism, and so does the window
between a replica subscribing and finishing its load. That window is also why the subscription
is taken out before the document is read rather than after.

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
