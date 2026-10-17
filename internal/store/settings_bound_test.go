package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/testsupport"
)

// A Stripe session carries its created time in whole seconds, so a
// term change made during that same second cannot be told apart from
// a checkout opened during it. Terms therefore take effect at the
// next whole second: a session is never charged terms that did not
// exist when Stripe stamped it, and two edits inside one second
// resolve to the later one.
func TestPlanUpdateTermsTakeEffectOnASecondBoundary(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Terms", Slug: "terms-" + suffix, Type: "desktop", FeedLicenseRequired: true}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "tp-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 365}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	// The plan has been selling 365 for a month.
	if _, err := s.DB.NewRaw("DELETE FROM plan_update_terms WHERE plan_id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw(
		`INSERT INTO plan_update_terms (id, plan_id, updates_days, effective_from, recorded_at)
		 VALUES (?, ?, 365, now() - interval '30 days', now() - interval '30 days')`, "seed-"+plan.ID, plan.ID,
	).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	// A plan's first row takes effect from the second the plan was
	// created in: there is nothing earlier to fall back to, and a
	// Payment Link session stamped in that second must find it.
	fresh := &model.Plan{ProductID: prod.ID, Name: "F", Slug: "tf-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 90}
	if err := s.CreatePlan(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	var created time.Time
	if err := s.DB.NewRaw("SELECT effective_from FROM plan_update_terms WHERE plan_id = ?", fresh.ID).Scan(ctx, &created); err != nil {
		t.Fatal(err)
	}
	if days, _, known, err := s.PlanTermsAt(ctx, fresh.ID, created); err != nil || !known || days != 90 {
		t.Fatalf("a session stamped in the creation second: got %d (known=%v, %v) want 90", days, known, err)
	}

	// Two edits inside the same second. They are stamped by the
	// database clock, so start them just after that clock turns a
	// second: begun at a random moment, the pair now and then straddled
	// a boundary and the test failed.
	var toNextSecond float64
	if err := s.DB.NewRaw("SELECT ceil(extract(epoch FROM clock_timestamp())) - extract(epoch FROM clock_timestamp())").Scan(ctx, &toNextSecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Duration(toNextSecond*float64(time.Second)) + 20*time.Millisecond)
	for _, days := range []int{180, 30} {
		plan.UpdatesDays = days
		if err := s.UpdatePlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	var boundary time.Time
	if err := s.DB.NewRaw("SELECT max(effective_from) FROM plan_update_terms WHERE plan_id = ?", plan.ID).Scan(ctx, &boundary); err != nil {
		t.Fatal(err)
	}
	if boundary.Nanosecond() != 0 {
		t.Fatalf("terms took effect mid-second: %v", boundary)
	}
	// A checkout Stripe stamped in the second the edits landed in
	// keeps the terms that were on offer when that second began.
	if days, _, known, err := s.PlanTermsAt(ctx, plan.ID, boundary.Add(-time.Second)); err != nil || !known || days != 365 {
		t.Fatalf("session in the edit's own second: got %d (known=%v, %v) want 365", days, known, err)
	}
	// From the boundary on, the last edit of that second applies.
	if days, _, known, err := s.PlanTermsAt(ctx, plan.ID, boundary); err != nil || !known || days != 30 {
		t.Fatalf("session after the boundary: got %d (known=%v, %v) want 30", days, known, err)
	}
}

// A plan's license type decides the whole shape of a license on it:
// its status, valid_until, update period and whether it carries a
// subscription row. A fulfilment that read the plan before an admin
// retyped it cannot be repaired into the new shape, so it is refused
// outright — the retry rebuilds from the plan as it reads then.
func TestCreateLicenseRefusesARetypedPlan(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.SetSetting(t, s.DB, SettingMaintenanceFeatures, "true")
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	// Gated and long drained: the retyped plan sells an update period.
	drained := time.Now().Add(-30 * 24 * time.Hour)
	prod := &model.Product{Name: "Retype", Slug: "retype-" + suffix, Type: "desktop",
		FeedLicenseRequired: true, FeedGatedAt: &drained}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "rt-" + suffix, LicenseType: "subscription", LicenseModel: "standard", BillingInterval: "month"}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	// The fulfilment's copy still says subscription; the admin retypes
	// the plan before it is paid.
	stale := *plan
	if _, err := s.DB.NewRaw("UPDATE plans SET license_type = 'perpetual', billing_interval = '', updates_days = 365 WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "retype-" + suffix + "@example.com",
		LicenseKey: "KEY-RT-" + suffix, Status: model.StatusActive, PaymentProvider: "stripe",
		UpdatesTermsSet: true}
	if err := s.CreateLicenseWithSubscription(ctx, lic, &stale); !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("create with a stale plan: %v want ErrPlanChanged", err)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM licenses WHERE email = ?", lic.Email).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a license was written from the stale plan: %d", n)
	}

	// Rebuilt from the plan as it reads now, the same sale goes
	// through — with the period the perpetual plan sells and no
	// subscription row.
	fresh, err := s.FindPlanByID(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	until := fresh.InitialUpdatesUntil(time.Now())
	if until == nil {
		t.Fatal("the retyped plan sells no period")
	}
	lic2 := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "retype2-" + suffix + "@example.com",
		LicenseKey: "KEY-RT2-" + suffix, Status: model.StatusActive, PaymentProvider: "stripe",
		UpdatesUntil: until, UpdatesTermsSet: true}
	if err := s.CreateLicenseWithSubscription(ctx, lic2, fresh); err != nil {
		t.Fatalf("create from the fresh plan: %v", err)
	}
	var subs int
	if err := s.DB.NewRaw("SELECT count(*) FROM subscriptions WHERE license_id = ?", lic2.ID).Scan(ctx, &subs); err != nil {
		t.Fatal(err)
	}
	if subs != 0 {
		t.Fatalf("a perpetual plan got a subscription row: %d", subs)
	}
	var stored *time.Time
	if err := s.DB.NewRaw("SELECT updates_until FROM licenses WHERE id = ?", lic2.ID).Scan(ctx, &stored); err != nil {
		t.Fatal(err)
	}
	if stored == nil {
		t.Fatal("the rebuilt license got updates for life on a bounded plan")
	}
}

// A plan edit that does not move the terms adds no history row, and a
// direct write that does move them adds one: the record follows the
// row, not the code path that wrote it. Otherwise a Payment Link
// session — authorised against this history — would be sold whatever
// the last API caller happened to hold.
func TestPlanTermsFollowTheRowNotTheCaller(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Terms2", Slug: "terms2-" + suffix, Type: "desktop", FeedLicenseRequired: true}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "P", Slug: "t2-" + suffix, LicenseType: "perpetual", LicenseModel: "standard", UpdatesDays: 365}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	// Another request shortens the period — a direct write, as an
	// older replica or an operator would make it. The history follows
	// it because the database records it, not this package.
	if _, err := s.DB.NewRaw("UPDATE plans SET updates_days = 30 WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	plan.Name = "Renamed"
	if err := s.UpdatePlan(ctx, plan, "name"); err != nil {
		t.Fatal(err)
	}
	var latest int
	if err := s.DB.NewRaw(
		"SELECT updates_days FROM plan_update_terms WHERE plan_id = ? ORDER BY effective_from DESC, recorded_at DESC LIMIT 1", plan.ID,
	).Scan(ctx, &latest); err != nil {
		t.Fatal(err)
	}
	if latest != 30 {
		t.Fatalf("an unrelated edit re-recorded the stale period: %d days", latest)
	}
}

// A Payment Link's session shape cannot tell a subscription from a
// trial — both carry a subscription id — so the history keeps the
// kind of licence the plan was selling, not only the period. What
// fulfilment does with it is the same comparison a session with
// frozen terms goes through.
func TestPlanTermsRememberTheLicenceType(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Kind", Slug: "kind-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "T", Slug: "kt-" + suffix, LicenseType: "trial", LicenseModel: "standard", TrialDays: 14}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	var opened time.Time
	if err := s.DB.NewRaw("SELECT effective_from FROM plan_update_terms WHERE plan_id = ?", plan.ID).Scan(ctx, &opened); err != nil {
		t.Fatal(err)
	}
	// The merchant turns the trial plan into a paid subscription.
	time.Sleep(1100 * time.Millisecond)
	plan.LicenseType, plan.TrialDays = "subscription", 0
	if err := s.UpdatePlan(ctx, plan, "license_type", "trial_days"); err != nil {
		t.Fatal(err)
	}
	if _, sold, known, err := s.PlanTermsAt(ctx, plan.ID, opened); err != nil || !known || sold != "trial" {
		t.Fatalf("a checkout opened before the retype: got %q (known=%v, %v) want trial", sold, known, err)
	}
	if _, sold, known, err := s.PlanTermsAt(ctx, plan.ID, time.Now().Add(time.Second)); err != nil || !known || sold != "subscription" {
		t.Fatalf("a checkout opened after the retype: got %q (known=%v, %v) want subscription", sold, known, err)
	}
	// An edit that touches neither the period nor the type adds no row.
	var before int
	if err := s.DB.NewRaw("SELECT count(*) FROM plan_update_terms WHERE plan_id = ?", plan.ID).Scan(ctx, &before); err != nil {
		t.Fatal(err)
	}
	plan.Name = "Renamed"
	if err := s.UpdatePlan(ctx, plan, "name"); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := s.DB.NewRaw("SELECT count(*) FROM plan_update_terms WHERE plan_id = ?", plan.ID).Scan(ctx, &after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("an unrelated edit added %d history row(s)", after-before)
	}
}

// The bound is what the drain is measured against, and it belongs to
// the install rather than to one replica: every start raises it, no
// start lowers it, and every check reads it back.
func TestFeedURLTTLBound(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	testsupport.LockSettings(t, s.DB)
	ctx := context.Background()
	key := "test_ttl_bound_" + time.Now().Format("150405.000000")
	defer func() {
		if _, err := s.DB.NewRaw("DELETE FROM settings WHERE key = ?", key).Exec(ctx); err != nil {
			t.Errorf("clean up %s: %v", key, err)
		}
	}()
	stored := func() string {
		v, err := s.GetSetting(ctx, key)
		if err != nil {
			t.Fatalf("read the bound: %v", err)
		}
		return v
	}

	// First start records what this replica runs — no guess about
	// what came before.
	if got, err := s.RaiseDurationSetting(ctx, key, 6*time.Hour); err != nil || got != 6*time.Hour {
		t.Fatalf("first start: %v %v", got, err)
	}
	if got := stored(); got != "6h0m0s" {
		t.Fatalf("bound not recorded: %q", got)
	}
	// Replicas starting at once keep the longest, whichever wins the
	// row.
	ttls := []time.Duration{time.Hour, 12 * time.Hour, 30 * time.Minute, 2 * time.Hour, 8 * time.Hour}
	var wg sync.WaitGroup
	for _, d := range ttls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.RaiseDurationSetting(ctx, key, d); err != nil {
				t.Errorf("raise %v: %v", d, err)
			}
		}()
	}
	wg.Wait()
	if got := stored(); got != "12h0m0s" {
		t.Fatalf("concurrent starts: %q want 12h0m0s", got)
	}
	// A restart with a shorter TTL keeps the recorded one: the links
	// signed under it are still out there.
	if got, err := s.RaiseDurationSetting(ctx, key, time.Minute); err != nil || got != 12*time.Hour {
		t.Fatalf("restart with a shorter TTL: %v %v", got, err)
	}
	// Lowering it by hand is the operator saying those links are
	// gone, and it stands until a replica configured higher starts.
	if err := s.SetSettings(ctx, map[string]string{key: "1h0m0s"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RaiseDurationSetting(ctx, key, time.Minute); err != nil || got != time.Hour {
		t.Fatalf("after lowering: %v %v", got, err)
	}
	if got, err := s.RaiseDurationSetting(ctx, key, 3*time.Hour); err != nil || got != 3*time.Hour {
		t.Fatalf("a replica configured higher raises it again: %v %v", got, err)
	}
	// A recorded value nobody can read is not the same as no record:
	// it may stand for a lifetime longer than this replica's, so
	// neither a start nor a drain check may quietly replace it with
	// the shorter one. Both fail until an operator fixes it.
	for _, bad := range []string{"", "   ", "not-a-duration", "0s", "-1h", (MaxDurationSetting + time.Hour).String()} {
		if err := s.SetSettings(ctx, map[string]string{key: bad}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RaiseDurationSetting(ctx, key, 2*time.Hour); err == nil {
			t.Fatalf("a start accepted %q as the recorded bound", bad)
		}
		if got := stored(); got != bad {
			t.Fatalf("a start overwrote %q with %q", bad, got)
		}
		if _, err := DurationSettingIn(ctx, s.DB, key, 2*time.Hour); err == nil {
			t.Fatalf("a drain check read %q as its own TTL", bad)
		}
	}
	// Nor may a replica record one it could not read back either.
	if _, err := s.RaiseDurationSetting(ctx, key, MaxDurationSetting+time.Hour); err == nil {
		t.Fatal("a start recorded a bound past the cap")
	}
	// Only a missing record falls back — to the caller's own TTL,
	// never to zero.
	if got, err := DurationSettingIn(ctx, s.DB, "test_ttl_bound_missing", 5*time.Hour); err != nil || got != 5*time.Hour {
		t.Fatalf("missing record: %v %v", got, err)
	}
}
