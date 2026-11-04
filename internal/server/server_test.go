package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aliraza777/burrowdb/internal/doc"
)

type client struct {
	t     *testing.T
	url   string
	token string
}

func newServer(t *testing.T, opts Options) (*client, *doc.DB) {
	t.Helper()
	db, err := doc.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(db, opts))
	t.Cleanup(func() { srv.Close(); db.Close() })
	return &client{t: t, url: srv.URL, token: opts.Token}, db
}

// do sends a request and decodes the JSON response into a map.
func (c *client) do(method, path, body string) (int, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.url+path, strings.NewReader(body))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%s %s: response is not JSON: %s", method, path, raw)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		c.t.Fatalf("%s %s: Content-Type %q", method, path, ct)
	}
	return res.StatusCode, out
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func TestDocumentsAndQueries(t *testing.T) {
	c, _ := newServer(t, Options{Version: "test"})
	if st, out := c.do("GET", "/health", ""); st != 200 || out["version"] != "test" {
		t.Fatalf("health: %d %v", st, out)
	}
	st, out := c.do("POST", "/collections/users/documents", `{"_id":"ann","name":"Ann","age":31}`)
	if st != 201 || out["id"] != "ann" {
		t.Fatalf("insert: %d %v", st, out)
	}
	st, out = c.do("POST", "/collections/users/documents", `[{"name":"Bob","age":27},{"name":"Cid","age":45,"tags":["x"]}]`)
	if ids, _ := out["ids"].([]any); st != 201 || len(ids) != 2 {
		t.Fatalf("insert many: %d %v", st, out)
	}
	if st, out = c.do("GET", "/collections/users/documents/ann", ""); st != 200 || out["name"] != "Ann" {
		t.Fatalf("get: %d %v", st, out)
	}
	if st, _ = c.do("PUT", "/collections/users/documents/ann", `{"name":"Ann","age":32}`); st != 200 {
		t.Fatalf("replace: %d", st)
	}
	st, out = c.do("POST", "/collections/users/find", `{"filter":{"age":{"$gt":30}},"sort":{"age":-1},"projection":{"name":1}}`)
	docs, _ := out["documents"].([]any)
	if st != 200 || len(docs) != 2 || docs[0].(map[string]any)["name"] != "Cid" || docs[1].(map[string]any)["age"] != nil {
		t.Fatalf("find: %d %v", st, out)
	}
	if st, out = c.do("POST", "/collections/users/find", ""); len(out["documents"].([]any)) != 3 {
		t.Fatalf("find with an empty body should return everything: %d %v", st, out)
	}
	if st, out = c.do("POST", "/collections/users/count", `{"filter":{"tags":"x"}}`); out["count"] != 1.0 {
		t.Fatalf("count: %d %v", st, out)
	}
	if st, out = c.do("POST", "/collections/users/indexes", `{"field":"age"}`); st != 201 {
		t.Fatalf("create index: %d %v", st, out)
	}
	// Enough other documents that the index is worth using (tiny collections scan).
	var more []string
	for i := range 40 {
		more = append(more, fmt.Sprintf(`{"age":%d}`, 100+i))
	}
	c.do("POST", "/collections/users/documents", "["+strings.Join(more, ",")+"]")
	if st, out = c.do("POST", "/collections/users/explain", `{"filter":{"age":32}}`); out["plan"] != "IXSCAN" || out["index"] != "age" {
		t.Fatalf("explain: %d %v", st, out)
	}
	if _, out = c.do("GET", "/collections/users/indexes", ""); len(out["indexes"].([]any)) != 1 {
		t.Fatalf("list indexes: %v", out)
	}
	if st, _ = c.do("DELETE", "/collections/users/indexes/age", ""); st != 200 {
		t.Fatalf("drop index: %d", st)
	}
	if st, out = c.do("DELETE", "/collections/users/documents/ann", ""); out["deleted"] != true {
		t.Fatalf("delete: %d %v", st, out)
	}
	if _, out = c.do("DELETE", "/collections/users/documents/ann", ""); out["deleted"] != false {
		t.Fatalf("second delete: %v", out)
	}
	if _, out = c.do("GET", "/collections", ""); len(out["collections"].([]any)) != 1 {
		t.Fatalf("collections: %v", out)
	}
	if _, out = c.do("DELETE", "/collections/users", ""); out["dropped"] != true {
		t.Fatalf("drop collection: %v", out)
	}
	if _, out = c.do("GET", "/collections", ""); len(out["collections"].([]any)) != 0 {
		t.Fatalf("collections after drop: %v", out)
	}
}

func TestErrorsHaveStatusAndCode(t *testing.T) {
	c, _ := newServer(t, Options{})
	c.do("POST", "/collections/u/documents", `{"_id":"a"}`)
	c.do("POST", "/collections/u/indexes", `{"field":"n"}`)
	cases := []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"GET", "/collections/u/documents/missing", "", 404, "not_found"},
		{"PUT", "/collections/u/documents/missing", `{}`, 404, "not_found"},
		{"POST", "/collections/u/documents", `{"_id":"a"}`, 409, "duplicate_id"},
		{"POST", "/collections/u/indexes", `{"field":"n"}`, 409, "index_exists"},
		{"DELETE", "/collections/u/indexes/nope", "", 404, "index_not_found"},
		{"POST", "/collections/u/documents", `not json`, 400, "bad_request"},
		{"POST", "/collections/u/documents", `[1, 2]`, 400, "bad_request"},
		{"POST", "/collections/bad%20name/documents", `{}`, 400, "bad_request"},
		{"POST", "/collections/u/documents", `{"_id":"has space"}`, 400, "bad_request"},
		{"POST", "/collections/u/find", `{"filter":{"x":{"$bogus":1}}}`, 400, "bad_request"},
		{"POST", "/collections/u/find", `{"limit":-1}`, 400, "bad_request"},
		{"POST", "/collections/u/find", `{"filter":{"n":1},"hint":"other"}`, 400, "bad_request"},
		{"POST", "/collections/u/indexes", `{"field":"$x"}`, 400, "bad_request"},
		{"PUT", "/collections/u/documents/a", `{"_id":"b"}`, 400, "bad_request"},
		{"POST", "/transaction", `{"ops":[{"op":"upsert"}]}`, 400, "bad_request"},
		{"GET", "/nope", "", 404, "not_found"},
		{"PATCH", "/collections/u/documents/a", `{}`, 404, "not_found"},
	}
	for _, tc := range cases {
		st, out := c.do(tc.method, tc.path, tc.body)
		if st != tc.status || errCode(out) != tc.code {
			t.Errorf("%s %s %s: got %d %q, want %d %q (%v)", tc.method, tc.path, tc.body, st, errCode(out), tc.status, tc.code, out)
		}
	}
}

func TestTransactionIsAllOrNothing(t *testing.T) {
	c, db := newServer(t, Options{})
	c.do("POST", "/collections/bank/documents", `[{"_id":"alice","balance":100},{"_id":"bob","balance":0}]`)
	st, out := c.do("POST", "/transaction", `{"ops":[
		{"op":"replace","collection":"bank","id":"alice","document":{"balance":50}},
		{"op":"replace","collection":"bank","id":"bob","document":{"balance":50}},
		{"op":"insert","collection":"log","document":{"_id":"t1","amount":50}},
		{"op":"delete","collection":"bank","id":"nobody"}
	]}`)
	results, _ := out["results"].([]any)
	if st != 200 || len(results) != 4 || results[2].(map[string]any)["id"] != "t1" || results[3].(map[string]any)["deleted"] != false {
		t.Fatalf("transaction: %d %v", st, out)
	}
	// The second op fails, so the first one must not be kept either.
	st, out = c.do("POST", "/transaction", `{"ops":[
		{"op":"replace","collection":"bank","id":"alice","document":{"balance":0}},
		{"op":"replace","collection":"bank","id":"ghost","document":{"balance":100}}
	]}`)
	if st != 404 || errCode(out) != "not_found" {
		t.Fatalf("failing transaction: %d %v", st, out)
	}
	if d, _ := db.Get("bank", "alice"); d["balance"] != int64(50) {
		t.Fatalf("a failed transaction changed alice: %v", d)
	}
}

func TestAuthAndLimits(t *testing.T) {
	c, _ := newServer(t, Options{Token: "s3cret"})
	if st, _ := c.do("GET", "/collections", ""); st != 200 {
		t.Fatalf("with the token: %d", st)
	}
	anon := &client{t: t, url: c.url}
	if st, out := anon.do("GET", "/collections", ""); st != 401 || errCode(out) != "unauthorized" {
		t.Fatalf("without a token: %d %v", st, out)
	}
	wrong := &client{t: t, url: c.url, token: "guess"}
	if st, _ := wrong.do("GET", "/collections", ""); st != 401 {
		t.Fatalf("wrong token: %d", st)
	}
	if st, _ := anon.do("GET", "/health", ""); st != 200 {
		t.Fatalf("health should not need a token: %d", st)
	}
	huge := `{"x":"` + strings.Repeat("y", MaxBody) + `"}`
	if st, out := c.do("POST", "/collections/u/documents", huge); st != 413 || errCode(out) != "too_large" {
		t.Fatalf("oversized body: %d %v", st, out)
	}
}
