package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/store"
)

// Invites grant roles through SetRoleAtomic: a new address becomes a
// member, only a request that changed the role writes the audit entry
// (so concurrent or repeated invites count once), and an actor who is
// not an owner is refused.
func TestInviteTeamMember(t *testing.T) {
	s := scratchStore(t)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	h := &AdminHandler{Store: s}

	insert := func(email, role string) string {
		t.Helper()
		id := store.NewID()
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO users (id, email, name, role) VALUES (?, ?, '', ?)", id, email, role); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ownerID := insert("owner@example.com", "owner")
	insert("helper@example.com", "admin")

	invite := func(actorEmail, actorID, body string) (int, string) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/admin/team", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("email", actorEmail)
		c.Set("user_id", actorID)
		h.InviteTeamMember(c)
		var out struct {
			Data struct {
				Role string `json:"role"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Data.Role
	}
	// Audit entries are written in the background; wait for them to land.
	audits := func(want int) int {
		t.Helper()
		var n int
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if err := s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE action = 'member_invited'").Scan(ctx, &n); err != nil {
				t.Fatal(err)
			}
			if n >= want {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
		if err := s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE action = 'member_invited'").Scan(ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if code, role := invite("owner@example.com", ownerID, `{"email":"New@Example.com"}`); code != http.StatusOK || role != "admin" {
		t.Fatalf("inviting a new address: %d role %q, want 200 admin", code, role)
	}
	if n := audits(1); n != 1 {
		t.Errorf("after one invite: %d audit entries, want 1", n)
	}
	if code, _ := invite("owner@example.com", ownerID, `{"email":"new@example.com"}`); code != http.StatusOK {
		t.Errorf("repeating the invite: %d, want 200", code)
	}

	codes := make([]int, 5)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = invite("owner@example.com", ownerID, `{"email":"burst@example.com","role":"owner"}`)
		}()
	}
	wg.Wait()
	for _, code := range codes {
		if code != http.StatusOK {
			t.Errorf("concurrent invites answered %v, want all 200", codes)
			break
		}
	}
	var burstRole string
	if err := s.DB.NewRaw("SELECT role FROM users WHERE email = 'burst@example.com'").Scan(ctx, &burstRole); err != nil || burstRole != "owner" {
		t.Errorf("burst@example.com is %q (err %v), want owner", burstRole, err)
	}
	if n := audits(2); n != 2 {
		t.Errorf("after a repeat and five concurrent invites of one address: %d audit entries, want 2", n)
	}

	// The handler's own owner check, made before the lock; the check made
	// again under the lock is covered by the store tests.
	if code, _ := invite("helper@example.com", "", `{"email":"sneaky@example.com","role":"owner"}`); code != http.StatusForbidden {
		t.Errorf("admin inviting: %d, want 403", code)
	}
	var sneaky int
	if err := s.DB.NewRaw("SELECT count(*) FROM users WHERE email = 'sneaky@example.com'").Scan(ctx, &sneaky); err != nil || sneaky != 0 {
		t.Errorf("refused invite left %d rows (err %v)", sneaky, err)
	}
}

// Removing a member: removals of one admin that all passed the handler's
// own check answer the same, and only the one that changed the role writes
// the audit entry. The test holds the team lock while they start, so every
// one of them is past that check and waiting on the lock.
func TestRemoveTeamMember(t *testing.T) {
	s := scratchStore(t)
	ctx := context.Background()
	gin.SetMode(gin.TestMode)
	h := &AdminHandler{Store: s}

	ownerID, adminID := store.NewID(), store.NewID()
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO users (id, email, name, role) VALUES (?, 'owner@example.com', '', 'owner'), (?, 'helper@example.com', '', 'admin')", ownerID, adminID); err != nil {
		t.Fatal(err)
	}

	hold, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", store.TeamLockKey); err != nil {
		t.Fatal(err)
	}

	bodies := make([]string, 5)
	codes := make([]int, 5)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodDelete, "/admin/team/"+adminID, nil)
			c.Params = gin.Params{{Key: "id", Value: adminID}}
			c.Set("email", "owner@example.com")
			c.Set("user_id", ownerID)
			h.RemoveTeamMember(c)
			codes[i], bodies[i] = w.Code, w.Body.String()
		}()
	}
	// Wait until all five are queued on the lock, then let them through.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var waiting int
		if err := s.DB.NewRaw("SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid = ? AND NOT granted", store.TeamLockKey).Scan(ctx, &waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == len(codes) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d removals reached the lock", waiting)
		}
	}
	if err := hold.Commit(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK || !strings.Contains(bodies[i], `"status":"removed"`) {
			t.Errorf("removal %d answered %d %s, want 200 removed", i, code, bodies[i])
		}
	}
	var n int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && n == 0; time.Sleep(50 * time.Millisecond) {
		if err := s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE action = 'member_removed'").Scan(ctx, &n); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if err := s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE action = 'member_removed'").Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d member_removed audit entries, want 1", n)
	}
}

// Each SetRoleAtomic refusal maps to its own status.
func TestTeamRoleError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		err  error
		code int
	}{
		{store.ErrLastOwner, http.StatusBadRequest},
		{store.ErrNotOwner, http.StatusForbidden},
		{store.ErrUserNotFound, http.StatusNotFound},
		{context.Canceled, http.StatusInternalServerError},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/admin/team", nil)
		teamRoleError(c, tc.err)
		if w.Code != tc.code {
			t.Errorf("%v: %d, want %d", tc.err, w.Code, tc.code)
		}
	}
}
