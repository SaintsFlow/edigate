package edge

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// TransportLog writes one JSON line for every request that goes through it (EDIGATE_TRANSPORT_LOG,
// protocol item O3): the time, the trace id, the MCP method, the tool, the body of the request and
// the body of the answer. It is for the stand only: the bodies hold the data of the benchmark,
// which is synthetic. It wraps the /mcp handler from outside, so a request that the edge refuses
// is in the log too.
type TransportLog struct {
	mu sync.Mutex
	f  *os.File
}

// OpenTransportLog opens (and creates) the file in append mode.
func OpenTransportLog(path string) (*TransportLog, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &TransportLog{f: f}, nil
}

// Close closes the file.
func (l *TransportLog) Close() error { return l.f.Close() }

// maxLogged is how much of a body is kept in a line.
const maxLogged = maxBody

// recorder copies the answer while it goes out.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if room := maxLogged - r.body.Len(); room > 0 {
		r.body.Write(p[:min(room, len(p))])
	}
	return r.ResponseWriter.Write(p)
}

// Flush keeps streaming answers working.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Wrap returns a handler that logs the requests it passes on to next.
func (l *TransportLog) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		head, _ := io.ReadAll(io.LimitReader(r.Body, maxLogged+1))
		// What was read goes back in front of what was not read, so the next handler sees the whole body.
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		l.write(w.Header().Get(traceHeader), head, rec)
	})
}

// write appends the line. A body that is JSON is kept as JSON, any other as a string.
func (l *TransportLog) write(trace string, request []byte, rec *recorder) {
	var call struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	_ = json.Unmarshal(request, &call)
	line, err := json.Marshal(struct {
		Time     string          `json:"time"`
		TraceID  string          `json:"trace_id"`
		Method   string          `json:"method"`
		Tool     string          `json:"tool"`
		Status   int             `json:"status"`
		Request  json.RawMessage `json:"request"`
		Response json.RawMessage `json:"response"`
	}{time.Now().UTC().Format(time.RFC3339Nano), trace, call.Method, call.Params.Name, rec.status, asJSON(request), asJSON(rec.body.Bytes())})
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.f.Write(append(line, '\n'))
}

// asJSON returns the bytes as a JSON value: themselves when they are valid JSON, else a string.
func asJSON(b []byte) json.RawMessage {
	if json.Valid(b) {
		return json.RawMessage(bytes.TrimSpace(b))
	}
	s, _ := json.Marshal(string(b))
	return s
}
