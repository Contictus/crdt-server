-- ycollab persistence schema.
--
-- The shape is the brief's: a document row carrying the latest snapshot, and an
-- append-only log of updates that arrived after it. Loading a document is the
-- snapshot followed by every remaining log row, in seq order.

CREATE TABLE IF NOT EXISTS documents (
    id            UUID PRIMARY KEY,
    owner_id      UUID NOT NULL,
    snapshot      BYTEA,
    snapshot_seq  BIGINT NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS doc_updates (
    doc_id      UUID   NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    seq         BIGINT GENERATED ALWAYS AS IDENTITY,
    payload     BYTEA  NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (doc_id, seq)
);
