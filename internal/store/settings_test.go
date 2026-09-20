package store

import "testing"

// SanitizeAdminSettings filters a raw key/value map down to the admin-editable
// allowlist: known keys with valid values pass through, unknown or internal
// keys are dropped with a warning, and a known key with a value of the wrong
// kind is a hard error (the caller surfaces it as 40002).
func TestSanitizeAdminSettings(t *testing.T) {
	clean, warnings, err := SanitizeAdminSettings(map[string]string{
		"retention_days":        "7",
		"log_bodies":            "true",
		"seeded_providers":      "1", // internal guard: never importable
		"auto_bind_new_models":  "0", // legacy internal key
		"some_future_key":       "x", // unknown: dropped with warning
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clean) != 2 || clean["retention_days"] != "7" || clean["log_bodies"] != "true" {
		t.Fatalf("clean map wrong: %+v", clean)
	}
	if len(warnings) != 3 {
		t.Fatalf("want 3 warnings for dropped keys, got %v", warnings)
	}

	// A known key with a value of the wrong kind is a hard error.
	for _, bad := range []map[string]string{
		{"retention_days": "abc"},
		{"log_bodies": "maybe"},
	} {
		if _, _, err := SanitizeAdminSettings(bad); err == nil {
			t.Fatalf("bad value must be rejected: %v", bad)
		}
	}
}
