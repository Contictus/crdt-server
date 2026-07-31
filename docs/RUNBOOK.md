# ycollab runbook

For whoever is on call. Every command here has been run; the backup and restore
section in particular is a transcript, not a suggestion.

## Where the state is

Three places, and knowing which one is which decides most incidents:

| What | Where | Survives |
| --- | --- | --- |
| The document being edited | a room's memory, one goroutine per document | nothing — it is a cache of the two below plus what clients hold |
| The durable copy | PostgreSQL: a `documents` row with a snapshot, plus an append-only `doc_updates` log | everything |
| Live fanout between replicas | Redis Pub/Sub | nothing, deliberately — a lost message is repaired by anti-entropy |

The clients are the fourth copy, and the most important one during an incident:
every connected browser holds the whole document. A server that restarts is a
resync, not a data loss, as long as the clients are still there.

## Deciding how bad it is

| Symptom | Severity | Why |
| --- | --- | --- |
| A replica is down, clients reconnect to another | low | reconnect and resync costs a diff |
| Redis is down | medium | replicas stop seeing each other; each one is internally consistent |
| Postgres is down | high | edits live only in memory and in the clients; a restart now loses them |
| `ycollab_frames_dropped_total` climbing | medium | clients are being disconnected to protect documents |
| Documents readable but wrong | stop and read "A document is wrong" below | do **not** restart; memory may hold the only good copy |

## Alerts

### YcollabDown

A replica is not being scraped.

1. `kubectl get pods -l app=ycollab` — is it restarting, or gone?
2. `kubectl logs <pod> --previous` — a startup failure says which flag or
   dependency: the server refuses to start on a bad `-database-url`, an
   unreachable Redis, a malformed `-webhook-url` or an unknown
   `-webhook-events` name.
3. If the process is up but not scraped, check `-admin-addr`. It defaults to
   `127.0.0.1:6060`, which is unreachable from another pod; the manifests set
   `0.0.0.0`.

Clients on that replica have already reconnected elsewhere. There is nothing to
recover: the room wrote its snapshot on the way out, or if it was killed, the
last flush window was lost and the clients replayed it on reconnect.

### YcollabWritesFailing

`ycollab_store_failed_total` is climbing. Edits exist only in memory and in the
clients that made them.

1. Is Postgres up and reachable? `kubectl exec <pod> -- /bin/sh -c 'nc -z <host> 5432'`.
2. Is it out of connections? `SELECT count(*) FROM pg_stat_activity;` against
   `max_connections`.
3. Is it out of disk? A full volume shows up here first.

**Do not restart the replicas while this is firing.** Their memory holds the
edits the database does not have. Fix the database and the next flush writes
everything queued; the write queue holds 4096 updates per document, and the room
blocks rather than dropping them when it fills. If you must restart, take a
per-document backup first (below) — that read comes from the room's memory,
which is exactly the copy at risk.

### YcollabDatabaseSlow

`ycollab_store_duration_seconds` p99 over a second. A slow database becomes a
stalled document, because the room blocks once its write queue fills.

1. `SELECT query, state, wait_event_type, now() - query_start AS age FROM
   pg_stat_activity WHERE state <> 'idle' ORDER BY age DESC LIMIT 10;`
2. Look for `ycollab_compactions_total` climbing at the same time. Compaction
   writes a whole snapshot and deletes the folded rows; a document being edited
   hard compacts every `-compact-after` updates (500 by default). Raising it
   trades a longer replay at load time for fewer big writes.

### YcollabIntegrationSlow

`ycollab_apply_duration_seconds` p99 over 50 ms. A healthy server is in the
microseconds — the benchmarks put one `ApplyUpdate` at about 630 ns.

Almost always CPU starvation rather than the engine: check the pod's CPU
throttling before anything else. If the CPU is fine, a single document has grown
far beyond what a person edits by hand, and `ycollab_update_bytes` will show it.

### YcollabDroppingFrames

`ycollab_frames_dropped_total` climbing. Each drop closes a connection with
1008: a client could not keep up and was disconnected to protect the document.

1. Is it one client or many? A single one is usually a browser tab that has been
   backgrounded and throttled.
2. Many, on one replica, means that replica is CPU-bound and its rooms cannot
   drain their outbound queues. Check `ycollab_apply_duration_seconds` and the
   pod's CPU.

Clients reconnect and resync, so this is a quality problem rather than a
correctness one — but a client that is dropped repeatedly is a person whose
editor keeps freezing.

### YcollabThrottlingHeavily

`ycollab_throttled_seconds_total` growing by more than a second per second. The
rate limiter is holding connections back; they experience it as the document
going slow.

1. `ycollab_throttled_total` divided by `ycollab_messages_received_total` says
   whether it is everybody or somebody.
2. Everybody: `-rate-messages` (200/s) is too low for what these clients do.
   A person typing produces ten to thirty a second, so a whole population over
   the limit means an automated client.
3. Somebody: one connection is flooding. It is being slowed, not disconnected,
   and only its own traffic is affected.

### YcollabClusterRepairing

Anti-entropy is answering more than one state vector a second: the Redis fanout
is losing messages rather than delivering them. Clients on different replicas
see each other late.

1. Check `ycollab_cluster_publish_failed_total` — if that is also climbing, this
   is a connection problem, not a loss problem.
2. Redis Pub/Sub drops messages for a subscriber whose output buffer fills.
   `redis-cli info clients` and `client-output-buffer-limit pubsub` in the Redis
   config are where that shows.
3. The documents are still correct. Anti-entropy is the mechanism working; a
   sustained stream means it is working constantly.

### YcollabClusterPublishFailing

A replica cannot publish. Its clients' edits are not reaching the other
replicas, and will only be repaired if the connection comes back.

If Redis is gone entirely, each replica is internally consistent and mutually
blind. Two people on the same document, on different replicas, will not see each
other and will both keep editing — which converges when the bus returns, because
that is what a CRDT is for. Nothing is lost; it is confusing while it lasts.

### YcollabHooksDropped / YcollabHooksFailing

Events are being discarded because the delivery queue is full, or given up on
after their retries. Whatever the webhook feeds is missing edits.

1. Is the receiver up? `ycollab_hooks_failed_total{reason}` says `transport`
   (could not reach it), `server_error`, `throttled` or `rejected` (a 4xx — it
   is refusing the request, so check the signature and the body).
2. Dropped rather than failed means the queue filled: the receiver is answering,
   too slowly. Raise `-webhook-queue` or make the receiver faster.
3. The events are gone and nothing will retry them. To catch up, read the
   documents directly: `GET /documents/{name}`.

## Common tasks

All on the admin listener, which defaults to `127.0.0.1:6060`.

```bash
ADMIN=127.0.0.1:6060

# What is this node doing?
curl -s $ADMIN/statsz | jq

# Read a document. The bytes are a Yjs update; Y.applyUpdate reads them.
curl -s $ADMIN/documents/my-doc > my-doc.bin

# Read it as text.
curl -s "$ADMIN/documents/my-doc?format=json" | jq

# Delete it, from memory and from the database. Refused with 409 while somebody
# is editing it.
curl -s -X DELETE $ADMIN/documents/my-doc

# Merge an update into it - restore, or seed from a template.
curl -s -X POST --data-binary @my-doc.bin $ADMIN/documents/my-doc
```

`POST` **merges**, it does not replace. These are CRDT updates: applying one adds
what it carries to what is there. Restoring over a document that has moved on
gives the union of the two. `DELETE` first when that is not what you want.

## Backup and restore

### The whole database

Everything is in two tables, so this is an ordinary Postgres backup.

```bash
pg_dump -U ycollab -d ycollab --format=custom --compress=9 > ycollab-$(date +%F).dump
```

Restoring:

```bash
psql  -U ycollab -d ycollab -c 'DROP TABLE IF EXISTS doc_updates, documents;'
pg_restore -U ycollab -d ycollab --no-owner --exit-on-error < ycollab-2026-07-31.dump
```

Two things worth knowing, both checked rather than assumed:

- **The identity sequence comes back.** `doc_updates.seq` is
  `GENERATED ALWAYS AS IDENTITY`, and the primary key is `(doc_id, seq)`. If a
  restore reset the sequence, the next write to any restored document would
  collide. It does not: after the round trip above, `max(seq)` and the
  sequence's `last_value` were both 76, and the next insert got 77. Verify it
  yourself after any restore:

  ```sql
  SELECT (SELECT max(seq) FROM doc_updates) AS max_seq,
         (SELECT last_value FROM pg_sequences WHERE sequencename = 'doc_updates_seq_seq') AS sequence;
  ```

- **Restore into a stopped cluster, or into an empty database.** A replica
  holding a document in memory does not reread it, so a restore underneath a
  running server is invisible until that room evicts — and when it does, it
  writes its own snapshot over what you restored.

The schema is created on startup if it is missing (`Migrate` is idempotent), so
restoring into a fresh database works without running anything by hand.

### One document

The read API is the backup, and the merge API is the restore. This is the
procedure `TestADocumentCanBeBackedUpAndRestored` runs on every CI build:

```bash
# 1. Take it. A resident document is read from the room, so this is exactly
#    what the connected clients are looking at.
curl -sf $ADMIN/documents/my-doc > my-doc.bin

# 2. Lose it, or corrupt it, or let somebody paste over it.

# 3. Put it back. DELETE first if the document has moved on and you want the
#    backup rather than the union of the two.
curl -sf -X DELETE $ADMIN/documents/my-doc
curl -sf -X POST --data-binary @my-doc.bin $ADMIN/documents/my-doc

# 4. Check.
curl -s "$ADMIN/documents/my-doc?format=json" | jq -r '.roots[].text'
```

A merge into a document people are already editing reaches them: the update is
broadcast to every connection and published to the other replicas, so nobody is
left building on a version the server no longer has.

### What a backup does not cover

- **Awareness.** Cursors and presence are never persisted, by design. They are
  rebuilt in seconds by the clients that are still connected.
- **The last flush window.** Up to `-flush-interval` (200 ms) of edits are in
  memory and not yet written. `-durable-writes` removes that window at the cost
  of a database round trip per update. The clients still hold them either way.
- **Redis.** Nothing in it needs backing up. It carries live fanout only.

## Restarts and upgrades

A rolling restart is cheap and safe:

1. Connections get a 1001 close frame, not a dead socket, because the HTTP
   server is drained before the rooms are told to stop.
2. Every room writes a final snapshot before its goroutine returns. `-shutdown-timeout`
   (10 s) bounds the drain.
3. Clients reconnect, send their state vector, and get back a diff. A restart
   costs one diff per client.

Roll one replica at a time and watch `ycollab_rooms_resident` on the others:
documents move to whichever replica the clients land on.

Rolling back is the same operation. The wire format is Yjs v1 and the schema has
not changed shape, so an older binary reads what a newer one wrote.

## Losing a dependency

**Redis goes away.** The replicas keep serving. Each is internally consistent;
they stop seeing each other. Rooms that were already up log publish failures and
keep going. When it comes back, anti-entropy reconciles them within one
`-anti-entropy` interval (15 s). A room that starts while Redis is down refuses
the document with 1011 rather than serving a copy that silently ignores everyone
on the other replicas.

**Postgres goes away.** Rooms already in memory keep serving; nothing is lost
while the process lives. New documents cannot be loaded, so a client asking for
one that is not resident is closed with 1011. Writes queue and then block. See
YcollabWritesFailing above: do not restart.

## Capacity

Measured on one machine, single replica, so treat them as shape rather than
promise:

| Shape | Throughput | p99 | Notes |
| --- | --- | --- | --- |
| 1000 clients, 100 rooms | 35 694 updates/s | 511 µs | delivery ratio 1.0000 |
| 200 clients, 2 rooms | 98 003 updates/s | 1.93 ms | 0 dropped frames |

One `ApplyUpdate` is about 630 ns; encoding a document as an update is about
26 µs. The engine is not the limit — network and fanout are.

Bounds a deployment should set, because the defaults are unlimited and that is
right for a laptop and wrong for anything reachable: `-max-conns`, `-max-rooms`.
The Kubernetes manifests in `deploy/k8s/` set both.

## A document is wrong

Rare, and worth being careful with, because the recovery options destroy
evidence.

1. **Do not restart the replica.** Its memory may hold the only correct copy.
2. Take a backup of what it currently holds: `curl -sf $ADMIN/documents/my-doc > now.bin`.
   That read comes from the room.
3. Compare it with what the database has, without disturbing the room. Ask a
   replica that does *not* hold the document: `X-Ycollab-Resident: false` on
   the response proves the answer came from the store.

   ```bash
   curl -sD- -o stored.bin http://other-replica:6060/documents/my-doc | grep -i resident
   ```

   With one replica, read the database directly rather than evicting the room,
   which would overwrite the stored copy with the one you are investigating.
4. If the memory copy is right and the stored one is wrong, the fix is to let
   the room write out: it does that on eviction, which happens
   `-idle-timeout` (5 min) after the last client leaves, and on a clean
   shutdown.
5. If the stored copy is right, `DELETE` the document to drop the room, then
   `POST` the good bytes back.

Keep `now.bin`. A Yjs update is self-contained and applies to an empty document,
so it is a complete record of what the server believed at that moment.
