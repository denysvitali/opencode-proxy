package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveModel(t *testing.T) {
	known := []string{"claude-sonnet-4-6", "gemini-3-flash", "big-pickle"}
	cfg := Config{Proxy: ProxyConfig{
		DefaultModel: "claude-sonnet-4-6",
		ModelMap:     map[string]string{"claude-special": "gemini-3.1-pro"},
	}}

	tests := []struct {
		name      string
		requested string
		known     []string
		want      string
	}{
		{"exact map hit", "claude-special", known, "gemini-3.1-pro"},
		{"known passthrough", "gemini-3-flash", known, "gemini-3-flash"},
		{"date suffix stripped", "claude-sonnet-4-6-20260101", known, "claude-sonnet-4-6"},
		{"unknown maps to default", "claude-3-5-haiku-20241022", known, "claude-sonnet-4-6"},
		{"no catalog passes through", "kimi-k3", nil, "kimi-k3"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cfg.ResolveModel(test.requested, test.known); got != test.want {
				t.Fatalf("ResolveModel(%q) = %q, want %q", test.requested, got, test.want)
			}
		})
	}
}

func TestReadOpenCodeAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	content := `{"opencode":{"type":"api","key":"sk-test"},"other":{"type":"api","key":"nope"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := readOpenCodeAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-test" {
		t.Fatalf("key = %q, want sk-test", key)
	}

	missing := filepath.Join(dir, "missing.json")
	key, err = readOpenCodeAuth(missing)
	if err != nil || key != "" {
		t.Fatalf("missing file: key=%q err=%v, want empty/nil", key, err)
	}
}
