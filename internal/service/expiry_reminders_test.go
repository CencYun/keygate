package service

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

func TestParseReminderDays(t *testing.T) {
	ok := map[string][]int{
		"7,3,1":       {7, 3, 1},
		" 1 , 7 ,3 ":  {7, 3, 1}, // trimmed and sorted largest first
		"3,3,1":       {3, 1},    // duplicates dropped
		"90":          {90},
		"30,14,7,3,1": {30, 14, 7, 3, 1},
		"7,,3":        {7, 3}, // stray comma ignored
	}
	for in, want := range ok {
		got, err := ParseReminderDays(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("ParseReminderDays(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", " , ", "0", "91", "-1", "1.5", "seven", "+7", "60,30,14,7,3,1"} {
		if _, err := ParseReminderDays(in); err == nil {
			t.Errorf("ParseReminderDays(%q) accepted", in)
		}
	}
	if got := FormatReminderDays([]int{7, 3, 1}); got != "7,3,1" {
		t.Errorf("FormatReminderDays = %q", got)
	}
}

func TestReminderWindow(t *testing.T) {
	days := []int{7, 3, 1}
	h := time.Hour
	for _, tc := range []struct {
		left time.Duration
		want int
		ok   bool
	}{
		{6*24*h + 23*h, 7, true}, // just inside 7 days
		{4 * 24 * h, 7, true},
		{3 * 24 * h, 3, true}, // boundary belongs to the smaller window
		{2 * 24 * h, 3, true}, // first seen at 2 days: 3-day reminder only
		{20 * h, 1, true},
		{8 * 24 * h, 0, false},
	} {
		got, ok := reminderWindow(days, tc.left)
		if got != tc.want || ok != tc.ok {
			t.Errorf("reminderWindow(%v) = %d, %v; want %d, %v", tc.left, got, ok, tc.want, tc.ok)
		}
	}
	if daysLeft(30*h) != 2 || daysLeft(2*h) != 1 || daysLeft(72*h) != 3 {
		t.Error("daysLeft rounds up to whole days, minimum 1")
	}
}

type reminderFixture struct {
	t    *testing.T
	s    *store.Store
	c    *ExpiryChecker
	plan *model.Plan
	prod *model.Product
}

func newReminderFixture(t *testing.T) *reminderFixture {
	s := openNotifyStore(t)
	ctx := context.Background()
	// setNotify takes the settings lock, which puts both rows back.
	setNotify(t, s, "license_expiring", "")
	_ = s.DeleteSetting(ctx, ReminderDaysSetting) // default 7,3,1

	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Remind", Slug: "remind-x-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Trial", Slug: "trial-x-" + suffix, LicenseType: "trial", LicenseModel: "standard", TrialDays: 14}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	// SMTP that refuses connections: sending fails fast in the background;
	// the tests read what was recorded.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	c := NewExpiryChecker(s, NewEmailService("127.0.0.1", strconv.Itoa(port), "", "", "keygate@test.local", slog.Default(), s), nil, slog.Default())
	return &reminderFixture{t: t, s: s, c: c, plan: plan, prod: prod}
}

func (f *reminderFixture) license(left time.Duration) *model.License {
	f.t.Helper()
	until := time.Now().Add(left).Truncate(time.Second)
	lic := &model.License{ProductID: f.prod.ID, PlanID: f.plan.ID, Email: "remind-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@example.com",
		LicenseKey: "KEY-" + strconv.FormatInt(time.Now().UnixNano(), 36), Status: model.StatusTrialing, ValidUntil: &until}
	if err := f.s.CreateLicense(context.Background(), lic); err != nil {
		f.t.Fatal(err)
	}
	// The reminders it triggers are queued; with no working SMTP they
	// would stay pending in the shared database and crowd the queue.
	f.t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", lic.Email).Exec(context.Background())
	})
	return lic
}

func (f *reminderFixture) tags(lic *model.License) []string {
	f.t.Helper()
	var tags []string
	if err := f.s.DB.NewRaw("SELECT tag FROM notifications WHERE license_id = ? ORDER BY tag", lic.ID).Scan(context.Background(), &tags); err != nil {
		f.t.Fatal(err)
	}
	return tags
}

func (f *reminderFixture) record(lic *model.License, tag string, sentAgo time.Duration) {
	f.t.Helper()
	if _, err := f.s.DB.NewRaw("INSERT INTO notifications (id, license_id, tag, sent_at) VALUES (gen_random_uuid()::text, ?, ?, now() - make_interval(secs => ?))",
		lic.ID, tag, sentAgo.Seconds()).Exec(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// remind runs the per-license reminder step on the fixture license only,
// never sweeping the shared database.
func (f *reminderFixture) remind(lic *model.License, days []int) bool {
	f.t.Helper()
	fresh, err := f.s.FindLicenseByID(context.Background(), lic.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.c.remindExpiring(context.Background(), fresh, days, time.Now())
}

func (f *reminderFixture) queued(lic *model.License) int {
	f.t.Helper()
	var n int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM email_queue WHERE to_addr = ?", lic.Email).Scan(context.Background(), &n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// A license first seen two days out gets the 3-day reminder only, once,
// through the durable mail queue.
func TestExpiryReminders_OneWindowAtATime(t *testing.T) {
	f := newReminderFixture(t)
	lic := f.license(2 * 24 * time.Hour)

	if !f.remind(lic, DefaultReminderDays) {
		t.Fatal("no reminder queued")
	}
	want := []string{ExpiryReminderTag(*lic.ValidUntil, 3)}
	if got := f.tags(lic); !slices.Equal(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	if f.queued(lic) != 1 {
		t.Fatalf("queued %d mails, want 1", f.queued(lic))
	}
	if f.remind(lic, DefaultReminderDays) || f.queued(lic) != 1 {
		t.Fatal("the same window was queued twice")
	}
}

// As the date approaches, the next (smaller) window is sent.
func TestExpiryReminders_MovesToTheNextWindow(t *testing.T) {
	f := newReminderFixture(t)
	lic := f.license(20 * time.Hour)
	f.record(lic, ExpiryReminderTag(*lic.ValidUntil, 3), 2*24*time.Hour)

	if !f.remind(lic, DefaultReminderDays) || !slices.Contains(f.tags(lic), ExpiryReminderTag(*lic.ValidUntil, 1)) {
		t.Fatalf("1-day reminder not sent: %v", f.tags(lic))
	}
}

// Changing the setting never sends a window further out than one that
// already went for the same date.
func TestExpiryReminders_NoLargerWindowAfterASmallerOne(t *testing.T) {
	f := newReminderFixture(t)
	lic := f.license(20 * time.Hour)
	f.record(lic, ExpiryReminderTag(*lic.ValidUntil, 1), time.Hour)
	if f.remind(lic, []int{14}) {
		t.Fatalf("a 14-day reminder followed the 1-day one: %v", f.tags(lic))
	}
}

// An extended license is reminded again, for its new date.
func TestExpiryReminders_ExtensionStartsOver(t *testing.T) {
	f := newReminderFixture(t)
	lic := f.license(20 * time.Hour)
	f.record(lic, ExpiryReminderTag(*lic.ValidUntil, 1), time.Hour)

	newDate := time.Now().Add(2 * 24 * time.Hour).Truncate(time.Second)
	if _, _, err := f.s.SetLicenseValidUntil(context.Background(), lic.ID, &newDate); err != nil {
		t.Fatal(err)
	}
	if !f.remind(lic, DefaultReminderDays) || !slices.Contains(f.tags(lic), ExpiryReminderTag(newDate, 3)) {
		t.Fatalf("no reminder for the new date: %v", f.tags(lic))
	}
}

// A reminder recorded in the old dateless form counts for the current
// date, so the upgrade does not repeat it; one from an earlier period
// does not.
func TestExpiryReminders_LegacyTagCounts(t *testing.T) {
	f := newReminderFixture(t)
	lic := f.license(2 * 24 * time.Hour)
	f.record(lic, "expiry_3d", 12*time.Hour) // sent inside the 3-day window
	if f.remind(lic, DefaultReminderDays) {
		t.Fatalf("legacy reminder was repeated: %v", f.tags(lic))
	}

	old := f.license(2 * 24 * time.Hour)
	f.record(old, "expiry_3d", 40*24*time.Hour)
	if !f.remind(old, DefaultReminderDays) {
		t.Fatalf("a stale legacy tag suppressed the reminder: %v", f.tags(old))
	}
}

// A claim whose sender died before queuing is taken over once its lease
// has run out, so the reminder is not lost.
func TestExpiryReminders_StaleClaimIsRetried(t *testing.T) {
	f := newReminderFixture(t)
	lic := f.license(2 * 24 * time.Hour)
	if _, err := f.s.DB.NewRaw(`INSERT INTO notifications (id, license_id, tag, sent_at, claimed_at)
		VALUES (gen_random_uuid()::text, ?, ?, NULL, now() - interval '1 hour')`,
		lic.ID, ExpiryReminderTag(*lic.ValidUntil, 3)).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.remind(lic, DefaultReminderDays) || f.queued(lic) != 1 {
		t.Fatalf("stale claim not retried: queued=%d", f.queued(lic))
	}
}

// A license gets either the renewal reminder (an active Stripe
// subscription that renews) or the expiry reminder (anything else) —
// never both, never neither.
func TestReminderSelectionsAreComplements(t *testing.T) {
	f := newReminderFixture(t)
	ctx := context.Background()
	inSet := func(list []*model.License, id string) bool {
		return slices.ContainsFunc(list, func(l *model.License) bool { return l.ID == id })
	}
	check := func(lic *model.License, wantExpiry, wantRenewal string) {
		t.Helper()
		now := time.Now()
		exp, err := f.s.FindLicensesForExpiryReminder(ctx, now, now.Add(3*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		ren, err := f.s.FindLicensesForRenewalReminder(ctx, now, now.Add(25*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if got := strconv.FormatBool(inSet(exp, lic.ID)); got != wantExpiry {
			t.Fatalf("expiry reminder selection = %s, want %s", got, wantExpiry)
		}
		if got := strconv.FormatBool(inSet(ren, lic.ID)); got != wantRenewal {
			t.Fatalf("renewal reminder selection = %s, want %s", got, wantRenewal)
		}
	}

	// Not billed by Stripe: it runs out unless someone extends it.
	manual := f.license(20 * time.Hour)
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET status = 'active' WHERE id = ?", manual.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	check(manual, "true", "false")

	stamp := func() time.Time {
		t.Helper()
		at, err := f.s.StripeReadStamp(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}

	// A Stripe subscription whose cancel state was never recorded (from
	// before this release): neither reminder until Stripe confirms it.
	stripeLic := f.license(20 * time.Hour)
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET status = 'active', stripe_subscription_id = ? WHERE id = ?",
		"sub_"+stripeLic.ID, stripeLic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncLicenseSubscriptionIn(ctx, f.s.DB, stripeLic.ID, f.plan, model.StatusActive, stripeLic.ValidUntil); err != nil {
		t.Fatal(err)
	}
	check(stripeLic, "false", "false")

	// Confirmed renewing.
	if err := f.s.SetSubscriptionCancelScheduled(ctx, stripeLic.ID, false, stamp()); err != nil {
		t.Fatal(err)
	}
	check(stripeLic, "false", "true")

	// Set to cancel at the period end: it now runs out.
	if err := f.s.SetSubscriptionCancelScheduled(ctx, stripeLic.ID, true, stamp()); err != nil {
		t.Fatal(err)
	}
	check(stripeLic, "true", "false")

	// Resumed: renews again.
	if err := f.s.SetSubscriptionCancelScheduled(ctx, stripeLic.ID, false, stamp()); err != nil {
		t.Fatal(err)
	}
	check(stripeLic, "false", "true")
}

// Switched off: the job returns before reading anything; nothing is
// recorded or queued.
func TestExpiryReminders_SwitchedOff(t *testing.T) {
	f := newReminderFixture(t)
	lic := f.license(2 * 24 * time.Hour)
	setNotify(t, f.s, "license_expiring", "false")
	f.c.SendExpiryReminders(context.Background())
	if got := f.tags(lic); len(got) != 0 || f.queued(lic) != 0 {
		t.Fatalf("switched off but recorded %v / queued %d", got, f.queued(lic))
	}
}
