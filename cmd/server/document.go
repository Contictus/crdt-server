package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/room"
)

// Headers a caller reads. The state vector is on every response, including the
// 304, because it is the version: a caller compares it with its own to decide
// whether to ask for a diff.
const (
	headerStateVector = "X-Ycollab-State-Vector"
	headerResident    = "X-Ycollab-Resident"
)

// readTimeout bounds the database half of a read.
const readTimeout = 30 * time.Second

// getDocument serves a document over HTTP, for the backends that want to read
// one without opening a WebSocket and speaking the sync protocol.
//
// It is on the admin listener, next to DELETE, and for the same reason: there
// is no per-document token that would authorise reading an arbitrary document,
// because the tokens this server understands are capabilities minted for
// editors. The listener is the authorisation, and the deployment decides who
// can reach it.
//
// The default body is the document as a Yjs update - the same bytes a client
// would receive from a sync with an empty state vector, so Y.applyUpdate reads
// it directly. That form is complete and authoritative. The JSON form is a
// convenience over it, and a lossy one - see rootView for what the wire format
// does not let it say.
func getDocument(manager *room.Manager, documents room.Persistence, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			http.Error(w, "no document name", http.StatusBadRequest)
			return
		}
		asJSON := wantsJSON(r)

		sv, err := requestedStateVector(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if asJSON && sv != nil {
			// A diff cannot be rendered: it is the difference from a document
			// this server has never seen, not a document. Saying so beats
			// quietly ignoring half the request.
			http.Error(w, "sv applies to the binary form only, not to format=json", http.StatusBadRequest)
			return
		}

		snapshot, err := readDocument(r.Context(), manager, documents, name, sv)
		switch {
		case errors.Is(err, room.ErrNoDocument):
			http.Error(w, "no such document", http.StatusNotFound)
			return
		case err != nil:
			log.Error("could not read document", "document", name, "err", err)
			http.Error(w, "could not read the document", http.StatusInternalServerError)
			return
		}

		// The state vector is the version, so it is the ETag. Two servers
		// holding the same document produce the same one, which is what makes
		// it usable behind a load balancer.
		etag := `"` + hex.EncodeToString(snapshot.StateVector) + `"`
		w.Header().Set(headerStateVector, base64.StdEncoding.EncodeToString(snapshot.StateVector))
		w.Header().Set(headerResident, boolString(snapshot.Resident))
		w.Header().Set("ETag", etag)
		// A document is not a public resource and the answer depends on who is
		// asking for what; caching it in a shared proxy is not something this
		// server should invite.
		w.Header().Set("Cache-Control", "no-store")
		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		if !asJSON {
			w.Header().Set("Content-Type", "application/octet-stream")
			if _, err := w.Write(snapshot.Update); err != nil {
				log.Debug("could not write document", "document", name, "err", err)
			}
			return
		}

		body, err := describe(name, snapshot)
		if err != nil {
			log.Error("could not render document", "document", name, "err", err)
			http.Error(w, "could not read the document", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			log.Debug("could not write document", "document", name, "err", err)
		}
	}
}

// readDocument asks the room if one is resident and the database otherwise.
func readDocument(ctx context.Context, manager *room.Manager, documents room.Persistence, name string, sv []byte) (room.Snapshot, error) {
	if resident := manager.Resident(name); resident != nil {
		snapshot, err := resident.Read(sv)
		if err == nil {
			return snapshot, nil
		}
		if !errors.Is(err, room.ErrClosed) {
			return room.Snapshot{}, err
		}
		// The room evicted itself while we were asking. It wrote its final
		// snapshot before it went, so the database now has what it had.
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	return room.Fetch(ctx, documents, name, sv)
}

// requestedStateVector parses ?sv=, which asks for the difference from a
// version the caller already has rather than the whole document.
func requestedStateVector(r *http.Request) ([]byte, error) {
	raw := r.URL.Query().Get("sv")
	if raw == "" {
		return nil, nil
	}
	// Standard base64 is what the webhook and the JSON body use, so that is
	// what a caller has to hand. URL-safe base64 is accepted too, because a
	// state vector goes in a query string and somebody will reasonably encode
	// it for one.
	if sv, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return sv, nil
	}
	sv, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("sv is not base64")
	}
	return sv, nil
}

// wantsJSON decides the representation. The query parameter wins over Accept,
// because it is the one somebody typed on purpose.
func wantsJSON(r *http.Request) bool {
	switch r.URL.Query().Get("format") {
	case "json":
		return true
	case "binary", "update":
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// etagMatches implements the part of If-None-Match this endpoint needs: a list
// of tags, or "*" for "whatever you have".
func etagMatches(header, etag string) bool {
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// document is the JSON representation.
type document struct {
	Document    string `json:"document"`
	StateVector string `json:"state_vector"`
	// Resident says whether this came from a room on this node or from the
	// database, and Clients is the connections that room had; it is omitted
	// when there was no room.
	Resident bool       `json:"resident"`
	Clients  *int       `json:"clients,omitempty"`
	Bytes    int        `json:"bytes"`
	Roots    []rootView `json:"roots"`
}

// rootView is one top-level shared type.
//
// There is no "type" field, and its absence is the honest answer rather than an
// omission: the v1 wire format never states what kind a root type is. Yjs
// decides when the client asks - doc.getText('x') and doc.getMap('x') read the
// same bytes two ways - so a server that only ever saw the updates cannot say
// which one was meant. Both readings are therefore offered, and a root that is
// a map reads as empty text, while a root that is text has no keys.
type rootView struct {
	Name string `json:"name"`
	// Text is the root read as a sequence: the concatenation of its string
	// content, which is what a YText or an XML root holds.
	Text string `json:"text"`
	// Keys are the root read as a map. Values are not included; a value can be
	// a nested shared type, and rendering those is the job the binary form
	// already does properly.
	Keys []string `json:"keys"`
}

// describe renders both readings of every root.
func describe(name string, snapshot room.Snapshot) (document, error) {
	doc := crdt.NewDoc(0)
	if err := doc.ApplyUpdate(snapshot.Update); err != nil {
		return document{}, err
	}
	out := document{
		Document:    name,
		StateVector: base64.StdEncoding.EncodeToString(snapshot.StateVector),
		Resident:    snapshot.Resident,
		Bytes:       len(snapshot.Update),
		Roots:       []rootView{},
	}
	if snapshot.Clients >= 0 {
		clients := snapshot.Clients
		out.Clients = &clients
	}
	names := doc.Roots()
	sort.Strings(names)
	for _, rootName := range names {
		t := doc.Get(rootName)
		keys := crdt.AsMap(t).Keys()
		if keys == nil {
			keys = []string{}
		}
		out.Roots = append(out.Roots, rootView{
			Name: rootName,
			Text: crdt.AsText(t).String(),
			Keys: keys,
		})
	}
	return out, nil
}
