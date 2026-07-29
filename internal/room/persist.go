package room

import (
	"context"
	"errors"
	"time"

	"github.com/mesutokul/ycollab/internal/store"
)

// Persistence is the part of internal/store a room needs. It is an interface so
// the room's own tests can run without a database; the integration tests use the
// real thing.
type Persistence interface {
	Load(ctx context.Context, id store.UUID) (*store.Document, error)
	Append(ctx context.Context, id store.UUID, payloads [][]byte) (int64, error)
	Compact(ctx context.Context, id store.UUID, snapshot []byte, watermark int64) error
}

const (
	// DefaultCompactAfter is the brief's threshold: fold the log into a new
	// snapshot once this many updates have accumulated.
	DefaultCompactAfter = 500
	// DefaultFlushInterval bounds how long an update can sit in memory before
	// it is written. It also bounds what a crash can lose - see the note on
	// durability in persist.
	DefaultFlushInterval = 200 * time.Millisecond
	// persistQueue is how many updates can be waiting to be written.
	persistQueue = 4096
	// maxBatch caps one INSERT.
	maxBatch = 256
	// persistTimeout bounds a single database call.
	persistTimeout = 30 * time.Second
)

type persistJob struct {
	// payload is an update to append, or nil for a compaction request.
	payload []byte
	// snapshot is the full document state to write, when this is a compaction.
	snapshot []byte
}

// load restores the document from the database: the snapshot, then every log
// row that is still there.
func (r *Room) load(ctx context.Context) error {
	doc, err := r.cfg.Store.Load(ctx, r.docID)
	if err != nil {
		return err
	}
	if doc.Snapshot != nil {
		if err := r.doc.ApplyUpdate(doc.Snapshot); err != nil {
			return err
		}
	}
	for _, u := range doc.Updates {
		if err := r.doc.ApplyUpdate(u); err != nil {
			// One corrupt row must not make the document unopenable: the rest
			// of the log still applies, and the clients still hold their own
			// copies. Losing an update is bad; refusing to serve the document
			// at all is worse.
			r.log.Error("skipping an update that would not apply", "err", err)
		}
	}
	r.sinceSnapshot = len(doc.Updates)
	// Start from where the log actually is. Without this a room that loads and
	// then evicts without anyone editing would try to compact at watermark 0,
	// be told its snapshot is stale, and leave the log unconsolidated forever.
	r.watermark = doc.LastSeq
	r.log.Info("loaded document",
		"snapshot", len(doc.Snapshot), "updates", len(doc.Updates), "pending", r.doc.PendingCount())
	return nil
}

// record queues an update to be written.
//
// It blocks when the queue is full, which stalls the room, which stalls the
// connections feeding it. That is the honest behaviour: the alternative is
// dropping updates that clients believe are saved. In practice the queue only
// fills if the database has been unreachable for a while, and at that point
// slowing down is the correct response.
func (r *Room) record(update []byte) {
	if r.jobs == nil {
		return
	}
	// The room broadcasts this slice too, and the writer keeps it until the
	// batch is flushed, so it is copied rather than shared.
	payload := make([]byte, len(update))
	copy(payload, update)
	r.jobs <- persistJob{payload: payload}

	r.sinceSnapshot++
	if r.sinceSnapshot >= r.cfg.CompactAfter {
		r.requestCompaction()
	}
}

// requestCompaction encodes the document and hands it to the writer.
//
// The room does the encoding because it owns the document; the writer decides
// the watermark, because it is the only thing that knows which log rows have
// actually been written. The snapshot can legitimately contain more than the
// watermark covers - the extra updates are still in the log and will simply be
// applied again on the next load, which costs nothing.
func (r *Room) requestCompaction() {
	if r.jobs == nil {
		return
	}
	snapshot, err := r.doc.EncodeStateAsUpdate(nil)
	if err != nil {
		r.log.Error("encode snapshot", "err", err)
		return
	}
	r.jobs <- persistJob{snapshot: snapshot}
	r.sinceSnapshot = 0
}

// persist is the room's database goroutine. It is the only thing that talks to
// the store, so writes for one document stay in order. The channel is passed in
// rather than read from the room, which lets the room drop its reference when
// it closes it.
//
// It deliberately does not take the server's context: on shutdown it has to
// finish flushing what it already accepted, and a cancelled context would turn
// a clean stop into the data loss the stop was trying to avoid. Each call is
// bounded by its own timeout instead.
func (r *Room) persist(jobs <-chan persistJob) {
	defer close(r.persistDone)

	var batch [][]byte
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
		seq, err := r.cfg.Store.Append(ctx, r.docID, batch)
		cancel()
		if err != nil {
			// The updates stay in memory and in every connected client. Say so
			// loudly rather than pretending the write happened.
			r.log.Error("could not write updates", "count", len(batch), "err", err)
		} else if seq > r.watermark {
			r.watermark = seq
		}
		batch = batch[:0]
	}

	ticker := time.NewTicker(r.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case job, ok := <-jobs:
			if !ok {
				flush()
				return
			}
			if job.snapshot != nil {
				// Everything queued before the snapshot request has to be on
				// disk first, or the watermark would cover rows that are not
				// there yet.
				flush()
				r.compact(job.snapshot)
				continue
			}
			batch = append(batch, job.payload)
			if len(batch) >= maxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (r *Room) compact(snapshot []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	err := r.cfg.Store.Compact(ctx, r.docID, snapshot, r.watermark)
	switch {
	case err == nil:
		r.log.Info("compacted", "watermark", r.watermark, "snapshot", len(snapshot))
	case errors.Is(err, store.ErrStaleSnapshot):
		// Another replica got there first with a newer snapshot. Nothing to do:
		// theirs contains everything ours does.
		r.log.Info("skipped compaction, a newer snapshot exists")
	default:
		r.log.Error("could not compact", "err", err)
	}
}

// finishPersisting flushes what is queued and waits for the writer to stop.
// Called from the room goroutine as it shuts down, which is the persist half of
// persist-on-evict.
func (r *Room) finishPersisting() {
	if r.jobs == nil {
		return
	}
	// A final snapshot means the next load is one row read rather than a replay
	// of the whole session.
	r.requestCompaction()
	close(r.jobs)
	r.jobs = nil
	<-r.persistDone
}
