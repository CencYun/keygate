package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/store"
)

// TestDemoteOwner_RaceTwoOwners exercises the exact bug the
// atomic implementation fixes: with 2 owners, two concurrent
// demotion attempts MUST NOT both succeed (which would leave zero
// owners). One wins; the other gets ErrLastOwner.
//
// Without the team lock (one advisory transaction lock around every
// role change) the old "count, then update" pattern would let both
// reads see count=2, both writes succeed, and leave no owner, locking
// everyone out of team management for good.
//
// We run many iterations to make the race window observable.
func TestDemoteOwner_RaceTwoOwners(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	// CRITICAL: the race only manifests when there are EXACTLY 2
	// owners total. With more, two concurrent demotions are
	// legitimate (org keeps an owner). We park every existing
	// owner as admin for the duration, then restore at the end.
	parked := parkAllExistingOwners(t, s, ctx)
	defer restoreParkedOwners(t, s, ctx, parked)

	const iterations = 30
	for i := range iterations {
		// Setup: insert 2 fresh owner rows. These are now the
		// ONLY 2 owners in the entire DB.
		emailA := uniqEmail(t, i, "ownerA")
		emailB := uniqEmail(t, i, "ownerB")
		idA := mustCreateOwner(t, s, ctx, emailA)
		idB := mustCreateOwner(t, s, ctx, emailB)

		// Two goroutines, each demoting one of the owners.
		var wg sync.WaitGroup
		results := make([]error, 2)
		wg.Add(2)
		ready := make(chan struct{})
		go func() {
			defer wg.Done()
			<-ready
			results[0] = demoteOwner(ctx, s, idA)
		}()
		go func() {
			defer wg.Done()
			<-ready
			results[1] = demoteOwner(ctx, s, idB)
		}()
		close(ready) // release both at once
		wg.Wait()

		// Invariant: count owners *for these two emails* — exactly one
		// of {emailA, emailB} must still hold role='owner'. Scoped by
		// email so other tests' owners (admin@keygate.dev seeded by
		// dev-login) don't pollute the count.
		var remaining int
		if err := s.DB.NewRaw(
			"SELECT COUNT(*) FROM users WHERE role='owner' AND email IN (?, ?)",
			emailA, emailB,
		).Scan(ctx, &remaining); err != nil {
			t.Fatalf("iter %d: count owners: %v", i, err)
		}
		if remaining != 1 {
			t.Fatalf("iter %d: expected exactly 1 of {A,B} to remain owner, got %d (errs: %v, %v)",
				i, remaining, results[0], results[1])
		}

		// Exactly one call should have returned ErrLastOwner; the
		// other should have returned nil (the successful demotion).
		errCount := 0
		lastOwnerCount := 0
		for _, err := range results {
			if err == nil {
				continue
			}
			errCount++
			if errors.Is(err, store.ErrLastOwner) {
				lastOwnerCount++
			} else {
				t.Fatalf("iter %d: unexpected error: %v", i, err)
			}
		}
		if errCount != 1 || lastOwnerCount != 1 {
			t.Fatalf("iter %d: expected exactly 1 ErrLastOwner; got errCount=%d lastOwnerCount=%d (errs: %v, %v)",
				i, errCount, lastOwnerCount, results[0], results[1])
		}

		// Cleanup the fixtures so the next iteration is clean.
		_, _ = s.DB.NewRaw("DELETE FROM users WHERE email IN (?, ?)", emailA, emailB).Exec(ctx)
	}
}

// TestDemoteOwner_HappyPath: single demotion of an owner
// when 2+ owners exist must succeed without error.
func TestDemoteOwner_HappyPath(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	emailA := uniqEmail(t, 0, "happyA")
	emailB := uniqEmail(t, 0, "happyB")
	idA := mustCreateOwner(t, s, ctx, emailA)
	_ = mustCreateOwner(t, s, ctx, emailB)
	defer func() {
		_, _ = s.DB.NewRaw("DELETE FROM users WHERE email IN (?, ?)", emailA, emailB).Exec(ctx)
	}()

	if err := demoteOwner(ctx, s, idA); err != nil {
		t.Fatalf("demotion failed: %v", err)
	}

	var roleA string
	if err := s.DB.NewRaw("SELECT role FROM users WHERE id = ?", idA).Scan(ctx, &roleA); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if roleA != "user" {
		t.Fatalf("expected role=user post-demote, got %q", roleA)
	}
}

// TestDemoteOwner_LastOwner: when there's only 1 owner in
// the whole table, the demotion must reject regardless of how many
// concurrent attempts happen.
func TestDemoteOwner_LastOwner(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	// First demote every existing owner except a freshly-minted one,
	// so we have a clean "exactly 1 owner" state. (Existing test
	// fixtures may have left others around.)
	emailSolo := uniqEmail(t, 0, "solo")
	idSolo := mustCreateOwner(t, s, ctx, emailSolo)
	defer func() {
		_, _ = s.DB.NewRaw("DELETE FROM users WHERE email=?", emailSolo).Exec(ctx)
	}()

	// Demote everyone else first (idempotent loop — leaves only Solo).
	var others []string
	_ = s.DB.NewRaw("SELECT id FROM users WHERE role='owner' AND id != ?", idSolo).Scan(ctx, &others)
	for _, oid := range others {
		// Use raw UPDATE to bypass our own atomic guard for this setup step.
		_, _ = s.DB.NewRaw("UPDATE users SET role='admin' WHERE id=?", oid).Exec(ctx)
	}
	defer func() {
		// Restore the other owners we demoted, so the rest of the
		// suite sees the original state.
		for _, oid := range others {
			_, _ = s.DB.NewRaw("UPDATE users SET role='owner' WHERE id=?", oid).Exec(ctx)
		}
	}()

	// Now Solo is the only owner. Any demotion of Solo
	// must error with ErrLastOwner.
	err := demoteOwner(ctx, s, idSolo)
	if !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("expected ErrLastOwner, got %v", err)
	}

	// And Solo must still be owner.
	var role string
	if err := s.DB.NewRaw("SELECT role FROM users WHERE id = ?", idSolo).Scan(ctx, &role); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if role != "owner" {
		t.Fatalf("solo demoted despite last-owner guard: role=%q", role)
	}

	// 10 concurrent attempts — all must fail, Solo still owner.
	var wg sync.WaitGroup
	var nilCount int32
	const N = 10
	wg.Add(N)
	ready := make(chan struct{})
	for range N {
		go func() {
			defer wg.Done()
			<-ready
			if err := demoteOwner(ctx, s, idSolo); err == nil {
				atomic.AddInt32(&nilCount, 1)
			}
		}()
	}
	close(ready)
	wg.Wait()
	if nilCount != 0 {
		t.Fatalf("%d concurrent demotions of last owner succeeded — should be 0", nilCount)
	}
}

// ─── helpers ────────────────────────────────────────────────

func uniqEmail(t *testing.T, iter int, tag string) string {
	t.Helper()
	// Encode the test name + iteration + a nanosecond suffix so
	// re-runs after a failed test (leftover rows in DB) don't trip
	// users_email_key. The cleanup step at the top of each test
	// also belt-and-suspenders this.
	return tag + "-" + t.Name() + "-" + itoa(iter) + "-" + itoa64(time.Now().UnixNano()) + "@last-owner-race.test"
}

// parkAllExistingOwners demotes every current owner to admin so the
// test can construct a clean "exactly N owners" state. Returns the
// list of IDs that were demoted so they can be restored at the end.
// The race we're testing only manifests at the boundary (count==2);
// stray test fixtures or dev-mode admins would otherwise mask it.
func parkAllExistingOwners(t *testing.T, s *store.Store, ctx context.Context) []string {
	t.Helper()
	var ids []string
	if err := s.DB.NewRaw("SELECT id FROM users WHERE role='owner'").Scan(ctx, &ids); err != nil {
		t.Fatalf("snapshot owners: %v", err)
	}
	for _, id := range ids {
		if _, err := s.DB.NewRaw("UPDATE users SET role='admin' WHERE id=?", id).Exec(ctx); err != nil {
			t.Fatalf("park owner %s: %v", id, err)
		}
	}
	return ids
}

func restoreParkedOwners(t *testing.T, s *store.Store, ctx context.Context, ids []string) {
	t.Helper()
	for _, id := range ids {
		_, _ = s.DB.NewRaw("UPDATE users SET role='owner' WHERE id=?", id).Exec(ctx)
	}
}

func mustCreateOwner(t *testing.T, s *store.Store, ctx context.Context, email string) string {
	t.Helper()
	id := store.NewID()
	if _, err := s.DB.NewRaw(
		"INSERT INTO users (id, email, name, role, created_at, updated_at) VALUES (?, ?, ?, 'owner', now(), now())",
		id, email, "Race Test",
	).Exec(ctx); err != nil {
		t.Fatalf("seed owner %s: %v", email, err)
	}
	return id
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 24)
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	digits := make([]byte, 0, 12)
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	if neg {
		digits = append(digits, '-')
	}
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}

// Two owners inviting each other as admin at the same moment: inviting
// an owner as admin demotes them, and used to skip the team lock, so
// both could succeed and leave no owner. Now one wins; the other is
// refused because its actor was just demoted.
func TestSetRoleAtomic_MutualInviteAsAdmin(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	for i := range 30 {
		emailA := uniqEmail(t, i, "mutualA")
		emailB := uniqEmail(t, i, "mutualB")
		idA := mustCreateOwner(t, s, ctx, emailA)
		idB := mustCreateOwner(t, s, ctx, emailB)
		t.Cleanup(func() { _, _ = s.DB.NewRaw("DELETE FROM users WHERE email IN (?, ?)", emailA, emailB).Exec(ctx) })

		var wg sync.WaitGroup
		results := make([]error, 2)
		ready := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-ready; _, _, results[0] = s.SetRoleAtomic(ctx, idA, idB, "admin") }()
		go func() { defer wg.Done(); <-ready; _, _, results[1] = s.SetRoleAtomic(ctx, idB, idA, "admin") }()
		close(ready)
		wg.Wait()

		var remaining int
		if err := s.DB.NewRaw("SELECT COUNT(*) FROM users WHERE role='owner' AND email IN (?, ?)", emailA, emailB).Scan(ctx, &remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 1 {
			t.Fatalf("iter %d: %d of {A,B} remain owner, want 1 (errs: %v, %v)", i, remaining, results[0], results[1])
		}
		refused := 0
		for _, err := range results {
			if errors.Is(err, store.ErrNotOwner) {
				refused++
			} else if err != nil {
				t.Fatalf("iter %d: unexpected error: %v", i, err)
			}
		}
		if refused != 1 {
			t.Fatalf("iter %d: want exactly one ErrNotOwner, got %v, %v", i, results[0], results[1])
		}
	}
}

// An owner demoted after the handler checked them, but before their own
// change runs, is refused under the lock.
func TestSetRoleAtomic_ActorMustStillBeOwner(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()

	idA := mustCreateOwner(t, s, ctx, uniqEmail(t, 0, "actorA"))
	idB := mustCreateOwner(t, s, ctx, uniqEmail(t, 0, "actorB"))
	idC := mustCreateOwner(t, s, ctx, uniqEmail(t, 0, "actorC"))
	defer func() { _, _ = s.DB.NewRaw("DELETE FROM users WHERE id IN (?, ?, ?)", idA, idB, idC).Exec(ctx) }()

	if _, _, err := s.SetRoleAtomic(ctx, idA, idB, "admin"); err != nil {
		t.Fatalf("owner A demoting B: %v", err)
	}
	if _, _, err := s.SetRoleAtomic(ctx, idB, idC, "admin"); !errors.Is(err, store.ErrNotOwner) {
		t.Fatalf("demoted B changing C: got %v, want ErrNotOwner", err)
	}
	var role string
	if err := s.DB.NewRaw("SELECT role FROM users WHERE id = ?", idC).Scan(ctx, &role); err != nil || role != "owner" {
		t.Fatalf("C is %q (err %v), want owner unchanged", role, err)
	}
	if _, _, err := s.SetRoleAtomic(ctx, idA, idB, "superuser"); err == nil {
		t.Fatal("an unknown role was accepted")
	}
	if prev, changed, err := s.SetRoleAtomic(ctx, idA, idB, "admin"); err != nil || changed || prev != "admin" {
		t.Errorf("repeating B's role: previous %q, changed %v, err %v, want admin and no change", prev, changed, err)
	}
	if _, _, err := s.SetRoleAtomic(ctx, idA, "no-such-user", "admin"); !errors.Is(err, store.ErrUserNotFound) {
		t.Errorf("missing target: got %v, want ErrUserNotFound", err)
	}
}

// demoteOwner removes a member from the team as the system would, with
// no acting user.
func demoteOwner(ctx context.Context, s *store.Store, id string) error {
	_, _, err := s.SetRoleAtomic(ctx, "", id, "user")
	return err
}
