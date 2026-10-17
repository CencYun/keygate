// Package testsupport holds helpers the test suites of several
// packages need from each other. It is imported only by tests.
package testsupport

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
)

// settingsLockKey is the advisory lock every test that writes a row of
// the settings table takes. Any number would do; it only has to be the
// same one everywhere.
const settingsLockKey = 918273645

// settingsLockWait is how long LockSettings waits for the lock. With
// packages run in parallel, a test can hold it for a minute while it
// works through a mail queue, and others queue up behind it.
const settingsLockWait = 5 * time.Minute

// settingsHeld records the tests holding the lock, so that a second
// LockSettings in the same test, from a helper say, does not wait on
// the first.
var settingsHeld sync.Map // settingsHolder -> struct{}

type settingsHolder struct {
	t  *testing.T
	db *bun.DB
}

// LockSettings gives a test the global settings table to itself: it
// serialises the tests that write it, and when the test ends it puts
// the table back as it found it, then releases the lock. Every test
// that writes a settings row must take it, even one writing a key of
// its own: putting the table back drops any row written meanwhile by
// a test that holds no lock.
//
// `go test ./...` runs packages in parallel and they all point at one
// database, so three packages flipping maintenance_features_enabled
// were stepping on each other: a test would set the switch on, and
// another package would set it off halfway through, failing the first
// one on a switch it never touched. Nothing in the product is wrong
// there — the setting is global by design — so the serialisation
// belongs here rather than in a per-test workaround. Putting the table
// back keeps a test from depending on what an earlier one left behind.
//
// The lock is taken on a connection of its own: an advisory lock lives
// on a session, and a pooled query may land on any connection. The
// same connection puts the table back, so that still works after the
// test's deferred Close of its store. Taking it again in the same test
// does nothing; a subtest is a test of its own, and waits on its
// parent's lock. The lock is polled for rather than waited on in one
// statement, which the driver's 10 second read timeout would cut short.
func LockSettings(t *testing.T, db *bun.DB) {
	t.Helper()
	holder := settingsHolder{t, db}
	if _, held := settingsHeld.LoadOrStore(holder, struct{}{}); held {
		return
	}
	// Registered first, so it runs after the cleanup that unlocks.
	t.Cleanup(func() { settingsHeld.Delete(holder) })
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("settings lock: %v", err)
	}
	for deadline := time.Now().Add(settingsLockWait); ; {
		var granted bool
		if err := conn.NewRaw("SELECT pg_try_advisory_lock(?)", settingsLockKey).Scan(ctx, &granted); err != nil {
			_ = conn.Close()
			t.Fatalf("settings lock: %v", err)
		}
		if granted {
			break
		}
		if time.Now().After(deadline) {
			_ = conn.Close()
			t.Fatalf("settings lock not granted in %v (held by a parent test?)", settingsLockWait)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Rows are kept as stored, so encrypted secrets come back unchanged.
	var saved string
	if err := conn.NewRaw("SELECT coalesce(jsonb_object_agg(key, value), '{}')::text FROM settings").Scan(ctx, &saved); err != nil {
		_, _ = conn.NewRaw("SELECT pg_advisory_unlock(?)", settingsLockKey).Exec(ctx)
		_ = conn.Close()
		t.Fatalf("settings lock: save settings: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.NewRaw(`WITH saved AS (SELECT key, value FROM jsonb_each_text(?::jsonb)),
			dropped AS (DELETE FROM settings WHERE key NOT IN (SELECT key FROM saved))
			INSERT INTO settings (key, value) SELECT key, value FROM saved
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
			WHERE settings.value IS DISTINCT FROM EXCLUDED.value`, saved).Exec(ctx); err != nil {
			t.Errorf("settings lock: put settings back: %v", err)
		}
		if _, err := conn.NewRaw("SELECT pg_advisory_unlock(?)", settingsLockKey).Exec(ctx); err != nil {
			t.Logf("settings unlock: %v", err)
		}
		_ = conn.Close()
	})
}

// SetSetting gives one settings row a value for the length of a test,
// so the test does not rely on another one having left it so. It takes
// LockSettings, which puts the row back when the test ends; a test may
// call it for several rows.
//
// It writes the row directly, which suits plain settings only: secret
// ones are stored encrypted by the store.
func SetSetting(t *testing.T, db *bun.DB, key, value string) {
	t.Helper()
	LockSettings(t, db)
	if _, err := db.ExecContext(context.Background(), "INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value", key, value); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
}
