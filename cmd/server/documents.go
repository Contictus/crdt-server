package main

// Listing the documents on the server, and moving one between owners.
//
// Both are on the admin listener, which is an operator surface: it is above the
// tenancy boundary by design, it needs its own credential, and everything on it
// is audited. A tenant-facing "list my documents" is the application's job - it
// knows its users, this server knows its documents - and this is what the
// application builds that on.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/mesutokul/ycollab/internal/room"
	"github.com/mesutokul/ycollab/internal/store"
)

// documentSummary is one document in a listing. Sizes, never content: a listing
// is for deciding what to fetch.
type documentSummary struct {
	// Name is what a client opens. It is empty for a row written before the
	// server recorded names, which is a document that can still be read by id
	// but that nothing here can tell you how to reach - see the runbook.
	Name string `json:"name"`
	ID   string `json:"id"`
	// OwnerID is the hashed owner. The tenant name it came from is not stored,
	// because the hash is one-way, so a caller that wants to display a name
	// matches this against the name it asked for.
	OwnerID string `json:"owner_id"`
	// Resident says whether a room on *this* replica holds the document.
	// Another replica may hold it and this one would not know.
	Resident      bool   `json:"resident"`
	UpdatedAt     string `json:"updated_at"`
	SnapshotBytes int64  `json:"snapshot_bytes"`
	Updates       int64  `json:"updates"`
}

type documentList struct {
	Documents []documentSummary `json:"documents"`
	// Next is the value to pass as ?after= for the following page, absent when
	// this was the last one.
	Next string `json:"next,omitempty"`
}

// listDocuments answers GET /documents.
//
// Without ?owner= it lists everything, which is the operator's view. With it,
// one tenant's documents - which is what an admin UI showing a customer's
// account needs, and what makes "delete this account's data" something anybody
// can actually carry out.
func listDocuments(manager *room.Manager, documents *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req := store.ListRequest{AllOwners: true}

		// An absent owner lists everything; an owner given as the empty string
		// lists the documents that belong to nobody. Those are different
		// questions, and Query().Get cannot tell them apart, so the presence of
		// the key is what decides.
		if owners, ok := r.URL.Query()["owner"]; ok && len(owners) > 0 {
			req.AllOwners = false
			req.Owner = store.OwnerID(owners[0])
		}
		if after := r.URL.Query().Get("after"); after != "" {
			cursor, err := store.ParseCursor(after)
			if err != nil {
				http.Error(w, "malformed after cursor", http.StatusBadRequest)
				return
			}
			req.After = cursor
		}
		if raw := r.URL.Query().Get("limit"); raw != "" {
			limit, err := strconv.Atoi(raw)
			if err != nil || limit <= 0 {
				http.Error(w, "limit must be a positive number", http.StatusBadRequest)
				return
			}
			req.Limit = limit
		}

		ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
		defer cancel()
		page, err := documents.List(ctx, req)
		if err != nil {
			log.Error("could not list documents", "err", err)
			http.Error(w, "could not list the documents", http.StatusInternalServerError)
			return
		}

		body := documentList{Documents: make([]documentSummary, 0, len(page.Documents)), Next: page.Next}
		for _, d := range page.Documents {
			body.Documents = append(body.Documents, documentSummary{
				Name:          d.Name,
				ID:            d.ID.String(),
				OwnerID:       d.Owner.String(),
				Resident:      d.Name != "" && manager.Resident(d.Name) != nil,
				UpdatedAt:     d.UpdatedAt.UTC().Format(time.RFC3339),
				SnapshotBytes: d.SnapshotBytes,
				Updates:       d.Updates,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			log.Debug("could not write the document list", "err", err)
		}
	}
}

// ownerRequest is the body of PUT /documents/{name}/owner.
type ownerRequest struct {
	// Owner is the tenant name. The empty string is a real value: it means
	// "owned by nobody", which is how a document is taken back out of a tenant.
	Owner string `json:"owner"`
}

// setOwner answers PUT /documents/{name}/owner.
//
// This is the only way an owner ever changes. Opening a document never
// reassigns it, so a deployment turning tenancy on finds its existing documents
// owned by nobody and reachable only by connections that claim no owner, until
// somebody says out loud whose they are. That is deliberately a decision an
// operator makes rather than a race the first tenant to connect wins.
//
// The room is evicted first. A resident room settled its owner when it opened,
// and would keep answering the old one until it fell out of memory - so an
// owner change that left the room running would take effect on this replica at
// some unpredictable time, and on the others immediately.
func setOwner(manager *room.Manager, documents *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			http.Error(w, "no document name", http.StatusBadRequest)
			return
		}
		var body ownerRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			http.Error(w, `body must be {"owner": "..."}`, http.StatusBadRequest)
			return
		}

		// Refused rather than queued: somebody is editing, and moving the
		// document under them would leave their connection attached to a room
		// whose owner no longer matches their grant.
		if !manager.Evict(name) {
			http.Error(w, "the document is in use", http.StatusConflict)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
		defer cancel()
		existed, err := documents.SetOwner(ctx, store.DocumentID(name), store.OwnerID(body.Owner))
		if err != nil {
			log.Error("could not set the owner", "document", name, "err", err)
			http.Error(w, "could not set the owner", http.StatusInternalServerError)
			return
		}
		if !existed {
			http.Error(w, "no such document", http.StatusNotFound)
			return
		}
		// A name is recorded when a document is created, and rows older than
		// that column have none. Reassigning one is the moment somebody is
		// looking at it and knows what it is called, so it is also the moment to
		// record the name - otherwise the document stays invisible in every
		// listing forever.
		named, err := documents.Name(ctx, store.DocumentID(name), name)
		if err != nil {
			log.Warn("could not record the document name", "document", name, "err", err)
		}
		log.Warn("document owner changed", "document", name, "owner", body.Owner, "named", named)
		w.WriteHeader(http.StatusNoContent)
	}
}

// wrongOwner reports whether an error is the tenancy boundary refusing.
func wrongOwner(err error) bool { return errors.Is(err, store.ErrWrongOwner) }
