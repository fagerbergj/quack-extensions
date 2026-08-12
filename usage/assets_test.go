package usage

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestExtension(t *testing.T) *extension {
	t.Helper()
	extVal, err := factory(newTestHost(), []byte("prometheus_url: http://prom:9090\ntempo_url: http://tempo:3200\ndefault_range: 1h\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	return extVal.(*extension)
}

func TestDashboardServes200WithEmbeddedAssets(t *testing.T) {
	e := newTestExtension(t)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<script src="app.js">`) {
		t.Error("dashboard HTML missing app.js script tag")
	}
	if !strings.Contains(body, `href="/assets/ext/v1/kit.css"`) {
		t.Error("dashboard HTML missing the design-kit stylesheet link")
	}
	if !strings.Contains(body, "defaultRangeSeconds:  3600") {
		t.Errorf("dashboard HTML did not inject the configured default range (1h = 3600s): %s", body)
	}
	if !strings.Contains(body, "http://tempo:3200") {
		t.Error("dashboard HTML did not inject the configured tempo_url")
	}
}

func TestDashboardOmitsTempoURLWhenUnconfigured(t *testing.T) {
	extVal, err := factory(newTestHost(), []byte("prometheus_url: http://prom:9090\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	e := extVal.(*extension)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if strings.Contains(rec.Body.String(), "http://tempo") {
		t.Error("dashboard HTML mentions a tempo URL that was never configured")
	}
}

func TestAppJSServesEmbeddedContent(t *testing.T) {
	e := newTestExtension(t)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want a javascript type", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "gen_ai_client_token_usage_total") {
		t.Error("app.js missing the token usage metric constant")
	}
	if !strings.Contains(body, "gen_ai_client_cost_total") {
		t.Error("app.js missing the cost metric constant")
	}
}

// TestDefaultRangeWindowConstant pins the documented default so a drive-by
// edit to the constant doesn't silently change quack.yaml's zero-config
// behavior without a test noticing.
func TestDefaultRangeWindowConstant(t *testing.T) {
	if defaultRangeWindow != 24*time.Hour {
		t.Errorf("defaultRangeWindow = %v, want 24h", defaultRangeWindow)
	}
}
