package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/store"
)

// scratchStore gives a test a migrated database of its own, for code that
// depends on what the whole database holds, such as setup.
func scratchStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	admin, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	t.Cleanup(func() { admin.Close() })
	name := fmt.Sprintf("keygate_scratch_test_%d", time.Now().UnixNano())
	if _, err := admin.DB.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.DB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	s, err := store.New(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return s
}

// Setup creates an owner without authentication, so it must stay closed
// on any database already in use, even one with no owner and no
// setup_complete: installs from before setup existed, and admins promoted
// through ADMIN_EMAILS, were exactly that, and anyone could take them
// over (#42).
func TestSetupClosedOnExistingInstall(t *testing.T) {
	s := scratchStore(t)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	h := NewSetupHandler(s, nil)

	status := func() bool {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/setup/status", nil)
		h.Status(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status: %d %s", w.Code, w.Body.String())
		}
		return strings.Contains(w.Body.String(), `"needed":true`)
	}
	initialize := func() int {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/setup/initialize", strings.NewReader(
			`{"admin_email":"intruder@example.com","admin_name":"Intruder","site_name":"Taken","product_name":"Taken","product_slug":"taken","product_type":"desktop"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h.Initialize(c)
		return w.Code
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.DB.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}

	if !status() {
		t.Fatal("an empty database should need setup")
	}

	exec("INSERT INTO users (id, email, name, role) VALUES (?, 'admin@example.com', 'Admin', 'admin')", store.NewID())
	if status() {
		t.Error("setup offered on an install with an admin and no owner")
	}
	if code := initialize(); code != http.StatusConflict {
		t.Errorf("initialize with an admin and no owner: %d, want 409", code)
	}
	if n, _ := s.CountOwners(ctx); n != 0 {
		t.Errorf("initialize created %d owners on an existing install", n)
	}

	exec("DELETE FROM users")
	exec("INSERT INTO products (id, name, slug, type) VALUES (?, 'Old', 'old', 'desktop')", store.NewID())
	if status() {
		t.Error("setup offered on an install with products and no admins")
	}
	if code := initialize(); code != http.StatusConflict {
		t.Errorf("initialize with products: %d, want 409", code)
	}

	// setup_complete alone closes it too.
	exec("DELETE FROM products")
	if !status() {
		t.Fatal("setup should reopen on a database emptied of admins and products")
	}
	exec("INSERT INTO settings (key, value) VALUES ('setup_complete', 'true')")
	if status() {
		t.Error("setup offered with setup_complete set")
	}
}

// Concurrent setups on an empty database: the lock and the check made
// again under it let exactly one through.
func TestSetupConcurrentInitialize(t *testing.T) {
	s := scratchStore(t)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	h := NewSetupHandler(s, nil)

	codes := make([]int, 8)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/setup/initialize", strings.NewReader(fmt.Sprintf(
				`{"admin_email":"u%d@example.com","admin_name":"U","site_name":"S","product_name":"P","product_slug":"p%d","product_type":"desktop"}`, i, i)))
			c.Request.Header.Set("Content-Type", "application/json")
			h.Initialize(c)
			codes[i] = w.Code
		}()
	}
	wg.Wait()

	created := 0
	for _, code := range codes {
		if code == http.StatusCreated {
			created++
		} else if code != http.StatusConflict {
			t.Errorf("unexpected status %d", code)
		}
	}
	owners, err := s.CountOwners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 || owners != 1 {
		t.Errorf("created %d, owners %d, want 1 and 1 (codes %v)", created, owners, codes)
	}
}

// An install with admins but no owner gets one promoted at startup, and
// ADMIN_EMAILS sign-ins become owner while no owner exists. It tests store
// functions but lives here for scratchStore: both need a database with no
// owner at all, which the shared store test database cannot promise.
func TestOwnerRepair(t *testing.T) {
	s := scratchStore(t)
	ctx := context.Background()
	insert := func(email, role string, at time.Time) string {
		t.Helper()
		id := store.NewID()
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO users (id, email, name, role, created_at) VALUES (?, ?, '', ?, ?)", id, email, role, at); err != nil {
			t.Fatal(err)
		}
		return id
	}
	roleOf := func(id string) string {
		t.Helper()
		u, err := s.FindUserByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return u.Role
	}

	if email, err := s.EnsureOwner(ctx, nil); err != nil || email != "" {
		t.Fatalf("empty database: promoted %q, err %v", email, err)
	}
	now := time.Now()
	later := insert("later@example.com", "admin", now)
	first := insert("first@example.com", "admin", now.Add(-time.Hour))
	listed := insert("listed@example.com", "admin", now.Add(time.Hour))
	// An admin listed in ADMIN_EMAILS wins over an earlier one.
	if email, err := s.EnsureOwner(ctx, []string{"absent@example.com", "listed@example.com"}); err != nil || email != "listed@example.com" {
		t.Fatalf("promoted %q, err %v, want the admin listed in ADMIN_EMAILS", email, err)
	}
	if email, err := s.EnsureOwner(ctx, nil); err != nil || email != "" {
		t.Errorf("second run promoted %q, err %v, want nothing", email, err)
	}
	// With none listed, the earliest admin.
	if _, err := s.DB.ExecContext(ctx, "UPDATE users SET role = 'admin' WHERE id = ?", listed); err != nil {
		t.Fatal(err)
	}
	if email, err := s.EnsureOwner(ctx, nil); err != nil || email != "first@example.com" {
		t.Fatalf("promoted %q, err %v, want the earliest admin", email, err)
	}
	if roleOf(first) != "owner" || roleOf(later) != "admin" || roleOf(listed) != "admin" {
		t.Errorf("roles after repair: first %s, later %s, listed %s", roleOf(first), roleOf(later), roleOf(listed))
	}

	// ADMIN_EMAILS sign-in: admin while an owner exists, owner otherwise.
	plain := insert("plain@example.com", "user", now)
	if role, err := s.PromoteAdminEmail(ctx, plain); err != nil || role != "admin" {
		t.Errorf("with an owner: role %q, err %v, want admin", role, err)
	}
	if _, err := s.DB.ExecContext(ctx, "DELETE FROM users WHERE role = 'owner'"); err != nil {
		t.Fatal(err)
	}
	fresh := insert("fresh@example.com", "user", now)
	if role, err := s.PromoteAdminEmail(ctx, fresh); err != nil || role != "owner" {
		t.Errorf("without an owner: role %q, err %v, want owner", role, err)
	}
	// Already raised: left as it is.
	if role, err := s.PromoteAdminEmail(ctx, plain); err != nil || role != "admin" {
		t.Errorf("an existing admin: role %q, err %v, want admin unchanged", role, err)
	}
}

// With ADMIN_EMAILS set, a fresh install stays open to setup until its
// admin first signs in; only a listed address can become owner, so a
// stranger who gets there first cannot claim it.
func TestSetupRestrictedToAdminEmails(t *testing.T) {
	s := scratchStore(t)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	h := NewSetupHandler(s, []string{"boss@example.com"})
	initialize := func(email string) int {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/setup/initialize", strings.NewReader(
			`{"admin_email":"`+email+`","admin_name":"X","site_name":"Acme","product_name":"Acme","product_slug":"acme","product_type":"desktop"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h.Initialize(c)
		return w.Code
	}

	for _, email := range []string{"intruder@example.com", "boſſ@example.com"} {
		if code := initialize(email); code != http.StatusForbidden {
			t.Errorf("unlisted email %q: %d, want 403", email, code)
		}
	}
	var users int
	if err := s.DB.NewRaw("SELECT count(*) FROM users").Scan(ctx, &users); err != nil || users != 0 {
		t.Fatalf("unlisted email left %d users (err %v)", users, err)
	}
	if code := initialize("Boss@Example.com"); code != http.StatusCreated {
		t.Errorf("listed email, other case: %d, want 201", code)
	}
}
