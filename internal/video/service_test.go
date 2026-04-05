package video

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveEnvReference(t *testing.T) {
	envs := map[string]string{
		"MINIMAX_API_KEY": "mini-key",
	}
	if got := resolveEnvReference("${MINIMAX_API_KEY}", envs); got != "mini-key" {
		t.Fatalf("resolveEnvReference placeholder mismatch: %q", got)
	}
	if got := resolveEnvReference("raw-key", envs); got != "raw-key" {
		t.Fatalf("resolveEnvReference raw mismatch: %q", got)
	}
}

func TestLoadEnvFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, ".env")
	content := "VIDEO_PROVIDER_API_KEY=${MINIMAX_API_KEY}\nMINIMAX_API_KEY='abc'\n# comment\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	values, err := loadEnvFile(path)
	if err != nil {
		t.Fatalf("loadEnvFile error: %v", err)
	}
	if values["VIDEO_PROVIDER_API_KEY"] != "${MINIMAX_API_KEY}" {
		t.Fatalf("unexpected VIDEO_PROVIDER_API_KEY: %q", values["VIDEO_PROVIDER_API_KEY"])
	}
	if values["MINIMAX_API_KEY"] != "abc" {
		t.Fatalf("unexpected MINIMAX_API_KEY: %q", values["MINIMAX_API_KEY"])
	}
}

func TestParseBool(t *testing.T) {
	if !parseBool("true") || !parseBool("1") || !parseBool("ON") {
		t.Fatalf("parseBool should accept true values")
	}
	if parseBool("false") || parseBool("") || parseBool("0") {
		t.Fatalf("parseBool should reject false values")
	}
}

func TestNormalizeModelName(t *testing.T) {
	if got := normalizeModelName("minimax/MiniMax-Hailuo-2.3"); got != "MiniMax-Hailuo-2.3" {
		t.Fatalf("normalizeModelName provider ref mismatch: %q", got)
	}
	if got := normalizeModelName("MiniMax-Hailuo-2.3"); got != "MiniMax-Hailuo-2.3" {
		t.Fatalf("normalizeModelName plain mismatch: %q", got)
	}
}
