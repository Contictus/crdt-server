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

-- Version history: the document as it stood, at points in time.
--
-- Each row is a complete Yjs update rather than a diff from the row before it.
-- A chain would be smaller and would make reading one version a walk through
-- every version before it, with no way to drop an old one - which is the
-- operation retention is made of. What keeps the size honest instead is
-- state_vector: a version is only written when it differs from the newest one,
-- so a document nobody edited gets one row however long the timer runs.
CREATE TABLE IF NOT EXISTS doc_versions (
    doc_id        UUID   NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    id            BIGINT GENERATED ALWAYS AS IDENTITY,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The version of the document, used to skip writing one that would say
    -- nothing new, and offered to callers so they can tell two versions apart
    -- without downloading both.
    state_vector  BYTEA  NOT NULL,
    payload       BYTEA  NOT NULL,
    -- Set when somebody asked for this version by hand, empty for the ones the
    -- timer took. It is what makes "before the migration" findable in a list of
    -- timestamps.
    label         TEXT   NOT NULL DEFAULT '',
    PRIMARY KEY (doc_id, id)
);

-- Listing and pruning both want the newest first for one document.
CREATE INDEX IF NOT EXISTS doc_versions_recent ON doc_versions (doc_id, id DESC);
