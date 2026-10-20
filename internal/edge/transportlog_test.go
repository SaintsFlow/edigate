package edge

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every request through the log leaves one line with the time, the trace id, the method, the
// tool, and both bodies; the handler behind it still sees the whole body; the file only grows.
func TestTransportLogWritesOneLinePerRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transport.jsonl")
	l, err := OpenTransportLog(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })

	var seen string
	h := l.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
		w.Header().Set(traceHeader, "trace-1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	const call = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"erp.get_price_list","arguments":{"sku":["SKU-1"]}}}`
	for range 2 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(call)))
		if seen != call || rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
			t.Fatalf("the handler saw %q, the client got %d %s", seen, rec.Code, rec.Body.String())
		}
	}
	// A body that is not JSON is kept as a string.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("not json")))

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 3 {
		t.Fatalf("%d lines, want 3", len(lines))
	}
	first := lines[0]
	req, _ := first["request"].(map[string]any)
	res, _ := first["response"].(map[string]any)
	if first["trace_id"] != "trace-1" || first["method"] != "tools/call" || first["tool"] != "erp.get_price_list" ||
		first["time"] == "" || req["method"] != "tools/call" || res["result"] == nil {
		t.Errorf("line = %v", first)
	}
	if lines[2]["request"] != "not json" || lines[2]["method"] != "" {
		t.Errorf("a body that is not JSON: %v", lines[2])
	}
}
