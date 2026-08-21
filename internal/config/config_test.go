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

func TestUsesAnthropicUpstream(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		model    string
		want     bool
	}{
		{name: "claude model by default", model: "claude-opus-5", want: true},
		{name: "free model by default", model: "x-preview-f-free", want: false},
		{name: "gemini by default", model: "gemini-3-flash", want: false},
		{name: "explicit exact match", patterns: []string{"big-pickle"}, model: "big-pickle", want: true},
		{name: "explicit prefix match", patterns: []string{"kimi-*"}, model: "kimi-k3", want: true},
		{name: "explicit list excludes claude", patterns: []string{"kimi-*"}, model: "claude-opus-5", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{Proxy: ProxyConfig{AnthropicModels: test.patterns}}
			if got := cfg.UsesAnthropicUpstream(test.model); got != test.want {
				t.Fatalf("UsesAnthropicUpstream(%q) = %v, want %v", test.model, got, test.want)
			}
		})
	}
}
