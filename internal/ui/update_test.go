// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylanlabs/logdiag-collector/internal/update"
)

func TestUpdateAPI(t *testing.T) {
	c, app := newAPI(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/latest.json" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"version": "9.0.0", "url": "https://logdiag.com/collector/", "notes": "新版本说明"}`))
	}))
	defer srv.Close()
	app.Update = &update.Checker{Client: srv.Client(), Sources: []update.Source{{Name: "logdiag.com", URL: srv.URL + "/latest.json"}}}

	var res update.Result
	if code := c.post("/api/update/check", nil, &res); code != http.StatusOK || !res.Newer || res.Latest != "9.0.0" ||
		res.Notes != "新版本说明" || !strings.HasPrefix(res.Page, "https://logdiag.com/collector/?") {
		t.Errorf("有新版本：%d %+v", code, res)
	}

	app.Update.Sources = []update.Source{{Name: "logdiag.com", URL: srv.URL + "/missing.json"}}
	var e struct{ Error string }
	if code := c.post("/api/update/check", nil, &e); code != http.StatusBadGateway || !strings.Contains(e.Error, "logdiag.com：还没有版本信息（HTTP 404）") {
		t.Errorf("读不到版本信息：%d %q", code, e.Error)
	}
}
