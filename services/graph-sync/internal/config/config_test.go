package config

import "testing"

// SPEC-W45 INT (F blocker): FALKORDB_PASSWORD must be read from env and
// surfaced on Config so the go-redis client can AUTH against the compose
// graph-db (--requirepass).
func TestLoadFalkorDBPassword(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		t.Setenv("FALKORDB_PASSWORD", "s3cret")
		cfg := Load()
		if cfg.FalkorDBPassword != "s3cret" {
			t.Fatalf("FalkorDBPassword = %q, want %q", cfg.FalkorDBPassword, "s3cret")
		}
	})
	t.Run("unset defaults empty (no AUTH)", func(t *testing.T) {
		cfg := Load()
		if cfg.FalkorDBPassword != "" {
			t.Fatalf("FalkorDBPassword = %q, want empty when FALKORDB_PASSWORD unset", cfg.FalkorDBPassword)
		}
	})
}
