package store

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// The trend's days are UTC days, like its from/to bounds, whatever time
// zone the database session runs in. Bucketing by the session's zone put
// an evening activation on the next day east of UTC, and an early one on
// the day before west of it.
func TestActivationTrendBucketsByUTCDay(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	seed, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer seed.Close()
	if err := seed.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Trend", Slug: "trend-" + suffix, Type: "desktop"}
	if err := seed.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Pro", Slug: "trend-pro-" + suffix, LicenseType: "perpetual", LicenseModel: "standard"}
	if err := seed.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "trend@example.com", LicenseKey: "KG-TREND-" + suffix, Status: model.StatusActive}
	if err := seed.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for i, at := range []time.Time{day.Add(30 * time.Minute), day.Add(12 * time.Hour), day.Add(23*time.Hour + 30*time.Minute)} {
		if _, err := seed.DB.ExecContext(ctx, "INSERT INTO activations (id, license_id, identifier, identifier_type, created_at) VALUES (?, ?, ?, 'device', ?)",
			NewID(), lic.ID, "trend-device-"+string(rune('a'+i)), at); err != nil {
			t.Fatal(err)
		}
	}

	for _, zone := range []string{"UTC", "Pacific/Kiritimati", "America/Los_Angeles"} {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("TimeZone", zone)
		u.RawQuery = q.Encode()
		s, err := New(u.String())
		if err != nil {
			t.Fatal(err)
		}
		var got string
		if err := s.DB.NewRaw("SELECT current_setting('TimeZone')").Scan(ctx, &got); err != nil || got != zone {
			s.Close()
			t.Fatalf("session time zone %q (%v), want %q", got, err, zone)
		}
		trend, err := s.GetActivationTrend(ctx, prod.ID, day, day.Add(24*time.Hour-time.Microsecond))
		s.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(trend) != 1 || trend[0].Date != "2026-10-14" || trend[0].Count != 3 {
			t.Errorf("session in %s: trend %+v, want 3 activations on 2026-10-14", zone, trend)
		}
	}
}
