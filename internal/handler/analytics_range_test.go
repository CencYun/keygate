package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// A "to" date covers that whole day, up to its last microsecond, and not
// the next midnight. Activations and usage are compared by timestamp, so
// a midnight bound dropped the whole day, usually today.
func TestAnalyticsToDateCoversTheDay(t *testing.T) {
	s := scratchStore(t)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)

	prod := &model.Product{Name: "Range", Slug: "range", Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Pro", Slug: "pro", LicenseType: "perpetual", LicenseModel: "standard"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "range@example.com", LicenseKey: "KG-RANGE", Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	inside := []time.Time{day, day.Add(12 * time.Hour), day.Add(24*time.Hour - time.Microsecond)}
	for i, at := range append(inside, day.Add(24*time.Hour)) {
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO activations (id, license_id, identifier, identifier_type, created_at) VALUES (?, ?, ?, 'device', ?)",
			store.NewID(), lic.ID, "device-"+string(rune('a'+i)), at); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO usage_events (id, license_id, feature, quantity, recorded_at) VALUES (?, ?, 'export', 1, ?)",
			store.NewID(), lic.ID, at); err != nil {
			t.Fatal(err)
		}
	}

	h := &AdminHandler{Store: s}
	get := func(fn func(*gin.Context), out any) {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/?from=2026-10-14&to=2026-10-14", nil)
		fn(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}

	var trend struct {
		Data struct {
			Trend []store.TrendPoint `json:"trend"`
		} `json:"data"`
	}
	get(h.AnalyticsActivationTrend, &trend)
	count := 0
	for _, p := range trend.Data.Trend {
		count += p.Count
	}
	if count != len(inside) {
		t.Errorf("activation trend counted %d, want %d (%+v)", count, len(inside), trend.Data.Trend)
	}

	var usage struct {
		Data struct {
			Features []store.FeatureUsageItem `json:"features"`
		} `json:"data"`
	}
	get(h.AnalyticsUsageTop, &usage)
	if len(usage.Data.Features) != 1 || usage.Data.Features[0].TotalUsage != int64(len(inside)) {
		t.Errorf("usage top = %+v, want export with %d", usage.Data.Features, len(inside))
	}
}

// With signing switched off, downloading a public key answers like
// generating one does, 503 SIGNING_DISABLED, not a 500.
func TestPublicKeyDownloadWhenSigningIsOff(t *testing.T) {
	s := scratchStore(t)
	gin.SetMode(gin.TestMode)
	prod := &model.Product{Name: "Unsigned", Slug: "unsigned", Type: "desktop"}
	if err := s.CreateProduct(context.Background(), prod); err != nil {
		t.Fatal(err)
	}
	h := NewReleaseSigningAdminHandler(nil, s)
	for name, fn := range map[string]func(*gin.Context){"pem": h.DownloadPublicKey, "tauri": h.DownloadPublicKeyTauri} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Params = gin.Params{{Key: "id", Value: prod.ID}}
		fn(c)
		var out struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != http.StatusServiceUnavailable || out.Error.Code != "SIGNING_DISABLED" {
			t.Errorf("%s: %d %s, want 503 SIGNING_DISABLED", name, w.Code, out.Error.Code)
		}
	}
}
