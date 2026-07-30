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
- **Phase 4 — horizontal scale.** Done: Redis Pub/Sub fanout with origin filtering, periodic
  anti-entropy, and a three-replica deployment behind Caddy.
- **Phase 5 — authorisation.** Done: HS256 tokens that name the document they open, read-only
  and read-write permissions, key rotation.
- **Phase 6 — operating it.** Done: Prometheus metrics and pprof on a separate admin listener,
  and a load bot that reports propagation latency.

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

### More than one replica

`-redis-url` makes a process one replica of a cluster rather than a server on its own. Replicas
relay each other's updates and cursors over Redis Pub/Sub, and each announces its state vector
every `-anti-entropy` seconds so a dropped message is repaired rather than becoming a permanent
divergence. Without it, two processes behind the same load balancer serve the same document
name as two unrelated documents, and the server says so at startup.

The whole thing — Postgres, Redis, three replicas and Caddy in front — comes up with:

```sh
docker compose -f deploy/docker-compose.cluster.yml up -d --build
```

Caddy is on <http://127.0.0.1:8080> and each replica is also published on 8081, 8082 and 8083,
which is what lets a test put one client on one replica and another on another; their admin
listeners are on 6081, 6082 and 6083. There is no
sticky session: any replica can serve any document, and a client that reconnects resyncs from
its state vector.

### Watching it

`-admin-addr` (default `127.0.0.1:6060`) serves the endpoints that are for operators rather
than for clients:

- `/metrics` — Prometheus. Connections, rooms, message counts by type, how long integrating an
  update takes, how long the database takes, and what the server refused.
- `/statsz` — the cluster counters as JSON: how many envelopes this node published and how many
  it filtered out as its own.
- `/debug/pprof` — the Go profiler. `-pprof=false` turns it off.

They are deliberately not on the port clients connect to. pprof dumps the heap, prints the
command line and will block the process for a thirty-second CPU profile on request, so the
deployment gets to decide who can reach it. `/healthz` is on both, because a load balancer
probes the port it sends traffic to.

### Tokens

`-jwt-secret` makes the server require a signed token. Without it every client may read and
write every document, which is fine for the demo and nothing else — the server warns about it
at startup.

A token is a JWT signed with HS256 that names the document it opens and the permission it
grants:

```json
{ "doc": "notes", "perm": "write", "sub": "ada", "exp": 1785400000 }
```

Naming the document is what makes it a capability rather than a login: a token that leaks opens
one document until it expires. `perm` is `read` or `write`; a read-only connection receives the
document and publishes its cursor, and is refused with a permission-denied message and a 1008
close if it tries to edit. An absent `perm` means `read`.

The token travels as `?token=...`, which is what `y-websocket`'s `params` option writes and the
only place a browser can put one — the WebSocket API cannot set a header. An `Authorization:
Bearer` header is accepted too, for clients that are not browsers. Because the token ends up in
URLs, expiry is required by default (`-jwt-require-expiry`) and can be capped
(`-jwt-max-lifetime`).

In a real deployment the application that knows who its users are mints these. For local use:

```sh
export YCOLLAB_JWT_SECRET=$(go run ./cmd/ycollab-token -gen-secret)
go run ./cmd/ycollab-token -doc demo -perm write -ttl 1h
go run ./cmd/ycollab-token -doc demo -perm read -url ws://127.0.0.1:8080   # a full URL
```

`-jwt-secret a,b` accepts both keys at once, which is how a key is rotated without an outage.

### The demo

```sh
cd web && npm install && npm run dev
```

Then open <http://localhost:5173/#demo> in two tabs and type in both. If the server requires
tokens, open <http://localhost:5173/?token=...#demo> instead — the page passes the token
through to the provider.

The header shows the connection state, the peer count and the document's state vector, so the
two tabs can be compared at a glance.

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

Against the cluster, the same harness spreads its clients over the replicas and reads each
replica's counters at the end, which is where "no update looped" is checked:

```sh
node tools/soak/soak.mjs --clients 6 --duration 300 \
  --urls ws://127.0.0.1:8081,ws://127.0.0.1:8082,ws://127.0.0.1:8083 \
  --stats http://127.0.0.1:6081,http://127.0.0.1:6082,http://127.0.0.1:6083
```

### Load

`cmd/ycollab-load` opens a lot of connections and reports how long an update takes to travel
from one client to another in the same room. It speaks the protocol directly rather than
running real Yjs clients — a real client costs a document and a provider, so a few hundred of
them make the generator the bottleneck. Correctness is what `tools/soak` is for.

```sh
go run ./cmd/server -addr 127.0.0.1:8080 &
go run ./cmd/ycollab-load -url ws://127.0.0.1:8080 -clients 1000 -rooms 100 -duration 30s -rate 4
```

On the development machine (Windows, 1000 connections over 100 rooms, four updates per second
each):

```
updates sent       118984 (3966/s)
updates delivered  1070856 (35694/s), expected 1070856
delivered ratio    1.0000
errors             0
propagation p99    511µs
propagation max    4.078ms
clock resolution   530.5µs
```

and with the fanout concentrated — 200 clients in two rooms, so every update goes to 99 peers:

```
updates delivered  1960101 (98003/s), expected 1960101
delivered ratio    1.0000
propagation p95    670µs
propagation p99    1.926ms
```

`delivered ratio 1.0000` is the part that matters: every update reached every other client in
its room, so nothing was dropped for backpressure. The server's own metrics put the mean cost
of integrating one update at about a microsecond.

The Go integration tests cover the same ground by starting real server processes; they need
Redis and skip without it:

```sh
docker compose -f deploy/docker-compose.yml up -d
YCOLLAB_TEST_REDIS_URL=redis://127.0.0.1:6380 go test ./... -race
```

## Layout

```
cmd/server            the server binary
cmd/ycollab-dump      re-encodes fixtures with the Go engine, for tools/verify
cmd/ycollab-token     mints tokens for local use
cmd/ycollab-load      opens many connections and measures propagation latency
internal/crdt         the CRDT engine; standard library only
internal/crdt/lib0    varUint, varInt, varString, varUint8Array and the any codec
internal/protocol     sync and awareness framing; pure bytes, no I/O
internal/room         one actor goroutine per document
internal/gateway      WebSocket lifecycle, pumps, backpressure
internal/store        PostgreSQL: snapshots and the append-only update log
internal/cluster      Redis Pub/Sub fanout between replicas
internal/auth         token verification: who may open which document, and how
internal/metrics      Prometheus collectors
tools/fixturegen      Node: generates the binary fixtures from real yjs
tools/verify          Node: applies Go-produced updates in real yjs
tools/soak            Node: drives real clients at a running server
web                   TipTap + y-websocket demo
deploy                docker-compose for local Postgres and Redis, plus the cluster
testdata/fixtures     committed binary fixtures
```
