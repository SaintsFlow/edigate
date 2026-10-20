package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SaintsFlow/edigate/internal/mode"
	"github.com/SaintsFlow/edigate/internal/store/storetest"
)

// /healthz names the mode the process plays.
func TestHealthzShowsTheMode(t *testing.T) {
	st, _ := storetest.New(t)
	for _, m := range mode.All {
		rec := httptest.NewRecorder()
		healthz(st, m)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var got struct{ Status, Mode string }
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || got.Status != "ok" || got.Mode != string(m) {
			t.Errorf("mode %s: %d %s (%v)", m, rec.Code, rec.Body.String(), err)
		}
	}
}
