// Package server exposes a burrowdb database over HTTP with a JSON API.
//
//	GET    /health
//	GET    /collections
//	DELETE /collections/{coll}
//	POST   /collections/{coll}/documents          a document, or an array of them
//	GET    /collections/{coll}/documents/{id}
//	PUT    /collections/{coll}/documents/{id}     replace
//	DELETE /collections/{coll}/documents/{id}
//	POST   /collections/{coll}/find               {"filter":…,"sort":…,"skip":…,"limit":…,"projection":…,"hint":…}
//	POST   /collections/{coll}/count              {"filter":…}
//	POST   /collections/{coll}/explain            same body as find
//	GET    /collections/{coll}/indexes
//	POST   /collections/{coll}/indexes            {"field":"age"}
//	DELETE /collections/{coll}/indexes/{field}
//	POST   /transaction                           {"ops":[{"op":"insert"|"replace"|"delete",…}]}, all or nothing
//
// Errors are {"error":{"code":"…","message":"…"}} with a matching HTTP status.
package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Aliraza777/burrowdb/internal/doc"
	"github.com/Aliraza777/burrowdb/internal/query"
)

// MaxBody bounds request bodies: room for a largest-size document plus JSON overhead.
const MaxBody = doc.MaxDocSize + 1<<20

// Options configure the handler.
type Options struct {
	// Token, if set, must be sent as "Authorization: Bearer <token>".
	Token   string
	Version string
}

type server struct {
	db   *doc.DB
	opts Options
}

// Handler returns the HTTP API for db.
func Handler(db *doc.DB, opts Options) http.Handler {
	s := &server{db: db, opts: opts}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /collections", s.listCollections)
	mux.HandleFunc("DELETE /collections/{coll}", s.dropCollection)
	mux.HandleFunc("POST /collections/{coll}/documents", s.insert)
	mux.HandleFunc("GET /collections/{coll}/documents/{id}", s.get)
	mux.HandleFunc("PUT /collections/{coll}/documents/{id}", s.replace)
	mux.HandleFunc("DELETE /collections/{coll}/documents/{id}", s.delete)
	mux.HandleFunc("POST /collections/{coll}/find", s.find)
	mux.HandleFunc("POST /collections/{coll}/count", s.count)
	mux.HandleFunc("POST /collections/{coll}/explain", s.explain)
	mux.HandleFunc("GET /collections/{coll}/indexes", s.listIndexes)
	mux.HandleFunc("POST /collections/{coll}/indexes", s.createIndex)
	mux.HandleFunc("DELETE /collections/{coll}/indexes/{field}", s.dropIndex)
	mux.HandleFunc("POST /transaction", s.transaction)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
	})
	return s.auth(mux)
}

func (s *server) auth(next http.Handler) http.Handler {
	if s.opts.Token == "" {
		return next
	}
	want := []byte("Bearer " + s.opts.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if r.URL.Path != "/health" && subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or wrong bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- responses ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeDocs writes {"documents":[…]} using doc.ToJSON, so _id comes first.
func writeDocs(w http.ResponseWriter, docs []map[string]any) {
	var buf bytes.Buffer
	buf.WriteString(`{"documents":[`)
	for i, d := range docs {
		if i > 0 {
			buf.WriteByte(',')
		}
		b, err := doc.ToJSON(d)
		if err != nil {
			writeFailure(w, err)
			return
		}
		buf.Write(b)
	}
	buf.WriteString("]}\n")
	w.Header().Set("Content-Type", "application/json")
	w.Write(buf.Bytes())
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// writeFailure maps an error to a status and a stable code.
func writeFailure(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, doc.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, doc.ErrIndexNotFound):
		status, code = http.StatusNotFound, "index_not_found"
	case errors.Is(err, doc.ErrDuplicateID):
		status, code = http.StatusConflict, "duplicate_id"
	case errors.Is(err, doc.ErrIndexExists):
		status, code = http.StatusConflict, "index_exists"
	case errors.Is(err, doc.ErrTooLarge):
		status, code = http.StatusRequestEntityTooLarge, "too_large"
	case errors.Is(err, doc.ErrBadCollection), errors.Is(err, doc.ErrBadID), errors.Is(err, doc.ErrBadField),
		errors.Is(err, doc.ErrIDMismatch), errors.Is(err, query.ErrBadHint), errors.As(err, new(*badRequest)):
		status, code = http.StatusBadRequest, "bad_request"
	}
	writeError(w, status, code, err.Error())
}

type badRequest struct{ msg string }

func (e *badRequest) Error() string { return e.msg }

// readBody reads a request body within MaxBody.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			return nil, doc.ErrTooLarge
		}
		return nil, &badRequest{"reading body: " + err.Error()}
	}
	return b, nil
}

// --- handlers ---

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.opts.Version})
}

func (s *server) listCollections(w http.ResponseWriter, _ *http.Request) {
	names, err := s.db.Collections()
	if err != nil {
		writeFailure(w, err)
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"collections": names})
}

func (s *server) dropCollection(w http.ResponseWriter, r *http.Request) {
	existed, err := s.db.DropCollection(r.PathValue("coll"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dropped": existed})
}

// insert accepts one document (responds {"id"}) or an array ({"ids"}, atomic).
func (s *server) insert(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		docs, err := parseDocArray(trimmed)
		if err != nil {
			writeFailure(w, err)
			return
		}
		ids, err := s.db.InsertMany(r.PathValue("coll"), docs)
		if err != nil {
			writeFailure(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"ids": ids})
		return
	}
	d, err := doc.FromJSON(body)
	if err != nil {
		writeFailure(w, &badRequest{err.Error()})
		return
	}
	id, err := s.db.Insert(r.PathValue("coll"), d)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func parseDocArray(b []byte) ([]map[string]any, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(b, &raws); err != nil {
		return nil, &badRequest{"invalid JSON array: " + err.Error()}
	}
	docs := make([]map[string]any, len(raws))
	for i, raw := range raws {
		d, err := doc.FromJSON(raw)
		if err != nil {
			return nil, &badRequest{fmt.Sprintf("document %d: %v", i, err)}
		}
		docs[i] = d
	}
	return docs, nil
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	d, err := s.db.Get(r.PathValue("coll"), r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	b, err := doc.ToJSON(d)
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(append(b, '\n'))
}

func (s *server) replace(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	d, err := doc.FromJSON(body)
	if err != nil {
		writeFailure(w, &badRequest{err.Error()})
		return
	}
	if err := s.db.Replace(r.PathValue("coll"), r.PathValue("id"), d); err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"replaced": true})
}

func (s *server) delete(w http.ResponseWriter, r *http.Request) {
	deleted, err := s.db.Delete(r.PathValue("coll"), r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (s *server) parseQuery(w http.ResponseWriter, r *http.Request) (query.Query, bool) {
	body, err := readBody(w, r)
	if err != nil {
		writeFailure(w, err)
		return query.Query{}, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	q, err := query.ParseQueryJSON(body)
	if err != nil {
		writeFailure(w, &badRequest{err.Error()})
		return query.Query{}, false
	}
	return q, true
}

func (s *server) find(w http.ResponseWriter, r *http.Request) {
	q, ok := s.parseQuery(w, r)
	if !ok {
		return
	}
	docs, err := query.Find(s.db, r.PathValue("coll"), q)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeDocs(w, docs)
}

func (s *server) count(w http.ResponseWriter, r *http.Request) {
	q, ok := s.parseQuery(w, r)
	if !ok {
		return
	}
	n, err := query.Count(s.db, r.PathValue("coll"), q.Filter)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": n})
}

func (s *server) explain(w http.ResponseWriter, r *http.Request) {
	q, ok := s.parseQuery(w, r)
	if !ok {
		return
	}
	ex, err := query.ExplainQuery(s.db, r.PathValue("coll"), q)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ex)
}

func (s *server) listIndexes(w http.ResponseWriter, r *http.Request) {
	idx, err := s.db.Indexes(r.PathValue("coll"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	out := make([]map[string]any, len(idx))
	for i, ix := range idx {
		out[i] = map[string]any{"field": ix.Field, "multikey": ix.Multikey}
	}
	writeJSON(w, http.StatusOK, map[string]any{"indexes": out})
}

func (s *server) createIndex(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	var req struct {
		Field string `json:"field"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeFailure(w, &badRequest{"body must be {\"field\": \"…\"}"})
		return
	}
	if err := s.db.CreateIndex(r.PathValue("coll"), req.Field); err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"field": req.Field})
}

func (s *server) dropIndex(w http.ResponseWriter, r *http.Request) {
	if err := s.db.DropIndex(r.PathValue("coll"), r.PathValue("field")); err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dropped": true})
}

// transaction applies a list of writes atomically:
//
//	{"ops":[
//	  {"op":"insert",  "collection":"users", "document":{…}},
//	  {"op":"replace", "collection":"users", "id":"…", "document":{…}},
//	  {"op":"delete",  "collection":"users", "id":"…"}
//	]}
//
// It responds with one result per op: {"id"} for inserts, {"deleted"} for deletes.
func (s *server) transaction(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	var req struct {
		Ops []struct {
			Op         string          `json:"op"`
			Collection string          `json:"collection"`
			ID         string          `json:"id"`
			Document   json.RawMessage `json:"document"`
		} `json:"ops"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeFailure(w, &badRequest{"invalid transaction: " + err.Error()})
		return
	}
	docs := make([]map[string]any, len(req.Ops))
	for i, op := range req.Ops {
		switch op.Op {
		case "insert", "replace":
			d, err := doc.FromJSON(op.Document)
			if err != nil {
				writeFailure(w, &badRequest{fmt.Sprintf("op %d: %v", i, err)})
				return
			}
			docs[i] = d
		case "delete":
		default:
			writeFailure(w, &badRequest{fmt.Sprintf("op %d: unknown op %q (insert, replace or delete)", i, op.Op)})
			return
		}
	}
	results := make([]map[string]any, len(req.Ops))
	err = s.db.Transaction(func(tx *doc.Tx) error {
		for i, op := range req.Ops {
			var err error
			switch op.Op {
			case "insert":
				var id string
				id, err = tx.Insert(op.Collection, docs[i])
				results[i] = map[string]any{"id": id}
			case "replace":
				err = tx.Replace(op.Collection, op.ID, docs[i])
				results[i] = map[string]any{"replaced": true}
			case "delete":
				var deleted bool
				deleted, err = tx.Delete(op.Collection, op.ID)
				results[i] = map[string]any{"deleted": deleted}
			}
			if err != nil {
				return fmt.Errorf("op %d (%s): %w", i, op.Op, err)
			}
		}
		return nil
	})
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
