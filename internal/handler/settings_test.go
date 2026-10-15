package handler

import (
	"slices"
	"testing"
)

// The three settings maps encode a policy, and the failure mode when
// they disagree is quiet: a key that is writable but hidden from
// GetSettings gives the dashboard a control it can save but never read
// back, and a server-owned key that slips into settingsWritable lets an
// operator overwrite the Stripe signing secret through the API.
func TestSettingsMapsAreConsistent(t *testing.T) {
	for key := range settingsServerOwned {
		if settingsWritable[key] {
			t.Errorf("%q is server-owned but also writable — the API must not accept it", key)
		}
		if settingsSecret[key] {
			t.Errorf("%q is server-owned; it is never exposed, so marking it secret is redundant", key)
		}
	}
	for key := range settingsSecret {
		if !settingsWritable[key] {
			t.Errorf("%q is write-only but not writable — nothing could ever set it", key)
		}
	}
}

// Regression guard for the save loop that broke once Stripe was
// configured: the dashboard seeds its form from GetSettings and PUTs
// the result back, so every key GetSettings returns must be one
// UpdateSettings will accept.
func TestGetSettingsOutputIsWritableBack(t *testing.T) {
	stored := map[string]string{
		"site_name":                  "Acme",
		"stripe_webhook_secret":      "whsec_live",
		"stripe_webhook_endpoint_id": "we_1",
		// Written by setup; must be neither returned nor writable.
		"setup_complete": "true",
	}

	for key, val := range stored {
		if settingsServerOwned[key] || settingsSecret[key] {
			continue
		}
		if !settingsWritable[key] {
			t.Errorf("GetSettings would return %q=%q but UpdateSettings rejects it", key, val)
		}
	}

	for _, key := range []string{"stripe_webhook_secret", "stripe_webhook_endpoint_id", "setup_complete"} {
		if !settingsSecret[key] && !settingsServerOwned[key] {
			t.Errorf("%q must never be returned by GetSettings", key)
		}
	}
}

// A fixed-choice setting has to be writable (otherwise the choice can
// never be made) and has to list the exact string its reader compares
// against. signupAllowed tests `mode != "licensed_only"`, so if that
// literal ever drifts out of the enum the API would reject the only
// value that actually turns the restriction on.
func TestSettingsEnumsAreUsable(t *testing.T) {
	for key, values := range settingsEnum {
		if !settingsWritable[key] {
			t.Errorf("%q has an allowed-value list but is not writable — the value could never be set", key)
		}
		if len(values) == 0 {
			t.Errorf("%q has an empty allowed-value list — every save would be rejected", key)
		}
	}

	if !slices.Contains(settingsEnum["signup_mode"], "licensed_only") {
		t.Error(`signup_mode must accept "licensed_only" — that is the value signupAllowed gates on`)
	}
	if !slices.Contains(settingsEnum["signup_mode"], "open") {
		t.Error(`signup_mode must accept "open" — the dashboard sends it to turn the restriction off`)
	}
}
