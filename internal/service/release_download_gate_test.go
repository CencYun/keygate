package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

// The download endpoint is where the maintenance period bites: a
// release published after updates_until is refused with
// UPDATES_EXPIRED when pinned, and skipped when picking "latest".
// Storage is disabled here, so a release that passes the gate shows
// up as STORAGE_DISABLED — the gate sits before the presign.
func TestGenerateDownload_MaintenancePeriod(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000")

	prod := &model.Product{Name: "DL Gate", Slug: "dlgate-" + suffix, Type: "desktop", FeedLicenseRequired: true}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Perp", Slug: "dlgate-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 3}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	publish := func(version string, publishedAt time.Time) {
		rel := &model.Release{ProductID: prod.ID, Version: version, Channel: model.ReleaseChannelStable}
		if err := s.CreateRelease(ctx, rel); err != nil {
			t.Fatalf("create release %s: %v", version, err)
		}
		art := &model.ReleaseArtifact{ReleaseID: rel.ID, Platform: "darwin-arm64"}
		if err := s.CreateArtifact(ctx, art); err != nil {
			t.Fatalf("create artifact: %v", err)
		}
		if err := s.UpdateArtifactFile(ctx, art.ID, "k/"+version, 10, strings.Repeat("ab", 32), "application/octet-stream"); err != nil {
			t.Fatalf("artifact file: %v", err)
		}
		if err := s.PublishRelease(ctx, rel.ID, false, ""); err != nil {
			t.Fatalf("publish %s: %v", version, err)
		}
		if _, err := s.DB.NewRaw("UPDATE releases SET published_at = ? WHERE id = ?", publishedAt, rel.ID).Exec(ctx); err != nil {
			t.Fatalf("set published_at: %v", err)
		}
	}
	cutoff := time.Now().Add(-10 * 24 * time.Hour)
	publish("1.0.0", cutoff.Add(-30*24*time.Hour)) // inside the period
	publish("2.0.0", cutoff.Add(5*24*time.Hour))   // after it

	mk := func(name string, updatesUntil *time.Time) string {
		lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: name + "-" + suffix + "@example.com",
			LicenseKey: "KEY-" + name + "-" + suffix, Status: model.StatusActive, UpdatesUntil: updatesUntil}
		if err := s.CreateLicense(ctx, lic); err != nil {
			t.Fatalf("create license: %v", err)
		}
		return "KEY-" + name + "-" + suffix
	}
	lapsed := mk("lapsed", &cutoff)
	unlimited := mk("life", nil)

	svc := NewReleaseService(ReleaseServiceConfig{Store: s, Logger: slog.Default()})
	code := func(key, version string) string {
		_, err := svc.GenerateDownload(ctx, DownloadInput{LicenseKey: key, ProductID: prod.ID, Platform: "darwin-arm64", Version: version})
		var ae *apperr.AppError
		if errors.As(err, &ae) {
			return ae.Code
		}
		if err != nil {
			return "ERR:" + err.Error()
		}
		return "OK"
	}
	if got := code(lapsed, "2.0.0"); got != "UPDATES_EXPIRED" {
		t.Fatalf("lapsed license pinning a newer release: %s", got)
	}
	if got := code(lapsed, "1.0.0"); got != "STORAGE_DISABLED" {
		t.Fatalf("lapsed license pinning an older release: %s", got)
	}
	// "latest" for a lapsed license is the newest release inside the
	// period, so the client still gets something it may install.
	if got := code(lapsed, ""); got != "STORAGE_DISABLED" {
		t.Fatalf("lapsed license asking for latest: %s", got)
	}
	rel, err := svc.findLatestPublished(ctx, prod.ID, model.ReleaseChannelStable, "darwin-arm64", &cutoff)
	if err != nil || rel.Version != "1.0.0" {
		t.Fatalf("latest inside period: %v %v", rel, err)
	}
	if got := code(unlimited, "2.0.0"); got != "STORAGE_DISABLED" {
		t.Fatalf("updates-for-life license refused: %s", got)
	}
	rel, err = svc.findLatestPublished(ctx, prod.ID, model.ReleaseChannelStable, "darwin-arm64", nil)
	if err != nil || rel.Version != "2.0.0" {
		t.Fatalf("latest without limit: %v %v", rel, err)
	}
	// The cutoff is applied in the query, before LIMIT: with room for
	// one row, the entitled older release is still the one returned.
	rows, err := s.ListPublishedReleasesForFeed(ctx, prod.ID, model.ReleaseChannelStable, "darwin-arm64", 1, &cutoff)
	if err != nil || len(rows) != 1 || rows[0].Version != "1.0.0" {
		t.Fatalf("cutoff after LIMIT would hide the entitled release: %v %v", rows, err)
	}
	// A stale date left on a license that moved to a subscription
	// plan does not gate anything: the period follows valid_until.
	subPlan := &model.Plan{ProductID: prod.ID, Name: "Sub", Slug: "dlgate-sub-" + suffix, LicenseType: "subscription", LicenseModel: "standard", MaxActivations: 3}
	if err := s.CreatePlan(ctx, subPlan); err != nil {
		t.Fatalf("create sub plan: %v", err)
	}
	moved := mk("moved", &cutoff)
	if _, err := s.DB.NewRaw("UPDATE licenses SET plan_id = ? WHERE license_key = ''", subPlan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET plan_id = ? WHERE email = ?", subPlan.ID, "moved-"+suffix+"@example.com").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if got := code(moved, "2.0.0"); got != "STORAGE_DISABLED" {
		t.Fatalf("subscription license gated by a stale date: %s", got)
	}
}

// The updater feeds carry the same limit when the client identifies
// its license: a lapsed license sees only entitled releases, a bad
// key sees nothing rather than the public list.
func TestFeedCutoff_MaintenancePeriod(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000")
	prod := &model.Product{Name: "Feed Gate", Slug: "feedgate-" + suffix, Type: "desktop", FeedLicenseRequired: true}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Perp", Slug: "feedgate-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 3}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	// Postgres keeps microseconds; Linux clocks give nanoseconds, so an
	// untruncated cutoff almost never compares Equal to the value read
	// back, which is cut to the microsecond on the way in.
	cutoff := time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Microsecond)
	for _, v := range []struct {
		version string
		at      time.Time
	}{{"1.0.0", cutoff.Add(-30 * 24 * time.Hour)}, {"2.0.0", cutoff.Add(5 * 24 * time.Hour)}} {
		rel := &model.Release{ProductID: prod.ID, Version: v.version, Channel: model.ReleaseChannelStable}
		if err := s.CreateRelease(ctx, rel); err != nil {
			t.Fatal(err)
		}
		art := &model.ReleaseArtifact{ReleaseID: rel.ID, Platform: "darwin-arm64"}
		if err := s.CreateArtifact(ctx, art); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateArtifactFile(ctx, art.ID, "k/"+v.version, 10, strings.Repeat("cd", 32), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
		if err := s.PublishRelease(ctx, rel.ID, false, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.NewRaw("UPDATE releases SET published_at = ? WHERE id = ?", v.at, rel.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	lapsed := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "lapsed-" + suffix + "@example.com", LicenseKey: "KEY-feed-lapsed-" + suffix, Status: model.StatusActive, UpdatesUntil: &cutoff}
	life := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "life-" + suffix + "@example.com", LicenseKey: "KEY-feed-life-" + suffix, Status: model.StatusActive}
	for _, l := range []*model.License{lapsed, life} {
		if err := s.CreateLicense(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewReleaseService(ReleaseServiceConfig{Store: s, Logger: slog.Default()})

	got, err := svc.FeedCutoff(ctx, "KEY-feed-lapsed-"+suffix, prod.ID)
	if err != nil || got == nil || !got.Equal(cutoff) {
		t.Fatalf("lapsed cutoff: %v %v", got, err)
	}
	if got, err := svc.FeedCutoff(ctx, "KEY-feed-life-"+suffix, prod.ID); err != nil || got != nil {
		t.Fatalf("lifetime cutoff: %v %v", got, err)
	}
	if _, err := svc.FeedCutoff(ctx, "KEY-nope", prod.ID); err == nil {
		t.Fatal("unknown key must not get a feed")
	}
	versions := func(rels []*model.Release) []string {
		var out []string
		for _, r := range rels {
			out = append(out, r.Version)
		}
		return out
	}
	rels, err := svc.ListForFeed(ctx, prod.ID, model.ReleaseChannelStable, "darwin-arm64", 20, got)
	if err != nil || len(rels) != 1 || rels[0].Version != "1.0.0" {
		t.Fatalf("licensed feed: %v %v", versions(rels), err)
	}
	rels, err = svc.ListForFeed(ctx, prod.ID, model.ReleaseChannelStable, "darwin-arm64", 20, nil)
	if err != nil || len(rels) != 2 {
		t.Fatalf("public feed: %v %v", versions(rels), err)
	}
}
