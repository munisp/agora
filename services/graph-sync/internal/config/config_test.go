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

// SPEC-W55 Scope A: GRAPH_SYNC_OCR_TOPIC follows the civic pattern
// (envStrExplicit) — an unset variable falls back to the default topic
// while an explicitly-set EMPTY value disables the OCR consumer.
func TestLoadOCRTopic(t *testing.T) {
	t.Run("unset defaults to opendesk.ocr.events", func(t *testing.T) {
		cfg := Load()
		if cfg.OCRTopic != "opendesk.ocr.events" {
			t.Fatalf("OCRTopic = %q, want %q when GRAPH_SYNC_OCR_TOPIC unset", cfg.OCRTopic, "opendesk.ocr.events")
		}
	})
	t.Run("explicit empty disables the OCR consumer", func(t *testing.T) {
		t.Setenv("GRAPH_SYNC_OCR_TOPIC", "")
		cfg := Load()
		if cfg.OCRTopic != "" {
			t.Fatalf("OCRTopic = %q, want empty when GRAPH_SYNC_OCR_TOPIC explicitly empty", cfg.OCRTopic)
		}
	})
	t.Run("explicit value honored", func(t *testing.T) {
		t.Setenv("GRAPH_SYNC_OCR_TOPIC", "custom.ocr.events")
		cfg := Load()
		if cfg.OCRTopic != "custom.ocr.events" {
			t.Fatalf("OCRTopic = %q, want %q", cfg.OCRTopic, "custom.ocr.events")
		}
	})
}
