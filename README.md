# ycollab

A self-hostable, Yjs wire-compatible collaboration server in Go.

The CRDT engine is written from scratch — YATA integration, struct store, state vectors,
delete sets and the lib0 codec, with no third-party imports in `internal/crdt` — and speaks the
Yjs v1 binary protocol byte for byte, so an unmodified `y-websocket` client works with no
client-side code at all.

Design decisions, the wire-format derivation and the open concerns are in
[DECISIONS.md](DECISIONS.md).

## Status

- **Phase 1 — CRDT core.** Done. Every committed fixture re-encodes byte-identically to the
  bytes Yjs produced, and `tools/verify/apply.mjs` accepts the Go output in a real `Y.Doc`.
- **Phase 2 — single-node WebSocket server.** Done: `internal/protocol`, `internal/room`,
  `internal/gateway`, `cmd/server`, the TipTap demo and the soak harness.
- **Phase 3 — persistence.** Done: PostgreSQL snapshot + append-only update log, compaction at
  500 updates, persist-on-evict, LRU cap on resident rooms.
- Phases 4-6 (Redis fanout, auth, load testing) are not started.

## Run it

```sh
docker compose -f deploy/docker-compose.yml up -d
go run ./cmd/server -addr :8080 -origins localhost:5173 \
  -database-url postgres://ycollab:ycollab@127.0.0.1:5433/ycollab
```

The document name is the URL path, so `ws://localhost:8080/my-doc` is document `my-doc`. Names
are mapped to document UUIDs by hashing, so they can be anything readable.

Without `-database-url` the server still works, but a document lives only as long as its room
is resident — five idle minutes by default. With it, updates are appended to a log, folded
into a snapshot every 500 updates, and written out when a room is evicted.

### The demo

```sh
cd web && npm install && npm run dev
```

Then open <http://localhost:5173/#demo> in two tabs and type in both. The header shows the
connection state, the peer count and the document's state vector, so the two tabs can be
compared at a glance.

### Tests

```sh
go test ./... -race
```

The store and the crash-recovery tests need a database and skip without one:

```sh
docker compose -f deploy/docker-compose.yml up -d
YCOLLAB_TEST_DATABASE_URL=postgres://ycollab:ycollab@127.0.0.1:5433/ycollab go test ./... -race
```

Those tests build the server, kill the process outright, restart it and reconnect, because a
graceful shutdown gets to flush and a crash does not.

Convergence is also checked against real clients. With a server running:

```sh
node tools/soak/soak.mjs --clients 6 --duration 300
```

Six real `y-websocket` clients edit the same document concurrently for five minutes, one of
them reconnecting every twenty seconds, and the run fails unless every client ends up with the
same text, the same state vector and the same view of who is in the room.

## Layout

```
cmd/server            the server binary
cmd/ycollab-dump      re-encodes fixtures with the Go engine, for tools/verify
internal/crdt         the CRDT engine; standard library only
internal/crdt/lib0    varUint, varInt, varString, varUint8Array and the any codec
internal/protocol     sync and awareness framing; pure bytes, no I/O
internal/room         one actor goroutine per document
internal/gateway      WebSocket lifecycle, pumps, backpressure
internal/store        PostgreSQL: snapshots and the append-only update log
tools/fixturegen      Node: generates the binary fixtures from real yjs
tools/verify          Node: applies Go-produced updates in real yjs
tools/soak            Node: drives real clients at a running server
web                   TipTap + y-websocket demo
deploy                docker-compose for local Postgres
testdata/fixtures     committed binary fixtures
```
