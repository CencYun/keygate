package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// The minimum supported message explains a minimum supported version.
// Sent where no version will be set, it is refused rather than dropped;
// turning the version off clears it.
func TestMinimumSupportedMessageNeedsVersion(t *testing.T) {
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
	gin.SetMode(gin.TestMode)
	prod := &model.Product{Name: "Floor", Slug: "floor-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour}
	update := func(body string) int {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/admin/products/"+prod.ID, strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "id", Value: prod.ID}}
		h.UpdateProduct(c)
		return w.Code
	}
	stored := func() (string, string) {
		p, err := s.FindProductByID(ctx, prod.ID)
		if err != nil {
			t.Fatal(err)
		}
		return p.MinimumSupportedVersion, p.MinimumSupportedMessage
	}

	for _, tc := range []struct {
		name, body       string
		code             int
		version, message string
	}{
		{"message with no version set", `{"minimum_supported_message":"Lonely"}`, http.StatusBadRequest, "", ""},
		{"version cleared with a message", `{"minimum_supported_version":"","minimum_supported_message":"Lonely"}`, http.StatusBadRequest, "", ""},
		{"version and message", `{"minimum_supported_version":"1.0.0","minimum_supported_message":"Please update."}`, http.StatusOK, "1.0.0", "Please update."},
		{"message alone while a version is set", `{"minimum_supported_message":"New wording"}`, http.StatusOK, "1.0.0", "New wording"},
		{"refused message leaves both alone", `{"minimum_supported_version":"","minimum_supported_message":"Lonely"}`, http.StatusBadRequest, "1.0.0", "New wording"},
		{"version cleared, message not sent", `{"minimum_supported_version":""}`, http.StatusOK, "", ""},
		{"both cleared, as the dashboard sends", `{"minimum_supported_version":"","minimum_supported_message":""}`, http.StatusOK, "", ""},
	} {
		if code := update(tc.body); code != tc.code {
			t.Errorf("%s: status %d, want %d", tc.name, code, tc.code)
		}
		if v, m := stored(); v != tc.version || m != tc.message {
			t.Errorf("%s: stored %q %q, want %q %q", tc.name, v, m, tc.version, tc.message)
		}
	}
}

// Setup answers with the rows as stored, not with objects built by hand
// whose unset fields read as zero (require_signing false, plan inactive,
// no checkout id, created_at in year 1). Setup runs once per database,
// so this test makes a database of its own.
func TestSetupAnswersStoredRows(t *testing.T) {
	s := scratchStore(t)
	ctx := context.Background()

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/setup/initialize", strings.NewReader(
		`{"admin_email":"owner@example.com","admin_name":"Owner","site_name":"Acme","product_name":"Acme App","product_slug":"acme-app","product_type":"desktop"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	NewSetupHandler(s, nil).Initialize(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Data struct {
			User    model.User    `json:"user"`
			Product model.Product `json:"product"`
			Plan    model.Plan    `json:"plan"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	prod, err := s.FindProductByID(ctx, out.Data.Product.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.FindPlanByID(ctx, out.Data.Plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Data.Product.RequireSigning || out.Data.Product.RequireSigning != prod.RequireSigning {
		t.Errorf("product require_signing: answered %v, stored %v", out.Data.Product.RequireSigning, prod.RequireSigning)
	}
	if !out.Data.Plan.Active || out.Data.Plan.CheckoutID != plan.CheckoutID || out.Data.Plan.MaxActivations != plan.MaxActivations {
		t.Errorf("plan: answered active=%v checkout=%q max=%d, stored active=%v checkout=%q max=%d",
			out.Data.Plan.Active, out.Data.Plan.CheckoutID, out.Data.Plan.MaxActivations, plan.Active, plan.CheckoutID, plan.MaxActivations)
	}
	for what, at := range map[string]time.Time{"user": out.Data.User.CreatedAt, "product": out.Data.Product.CreatedAt, "plan": out.Data.Plan.CreatedAt} {
		if at.Year() < 2000 {
			t.Errorf("%s created_at answered as %v", what, at)
		}
	}
}
