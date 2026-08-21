package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/viper"
)

const (
	// ZenBase is the public OpenCode Zen API base URL.
	ZenBase = "https://opencode.ai/zen/v1"
)

var dateSuffix = regexp.MustCompile(`-\d{8}$`)

type Config struct {
	AuthFile  string       `mapstructure:"auth_file"`
	BaseURL   string       `mapstructure:"base_url"`
	APIKey    string       `mapstructure:"api_key"`
	LogLevel  string       `mapstructure:"log_level"`
	LogFormat string       `mapstructure:"log_format"`
	Server    ServerConfig `mapstructure:"server"`
	Proxy     ProxyConfig  `mapstructure:"proxy"`
}

type ServerConfig struct {
	Listen        string `mapstructure:"listen"`
	APIKey        string `mapstructure:"api_key"`
	AllowInsecure bool   `mapstructure:"allow_insecure"`
	MaxBodyBytes  int64  `mapstructure:"max_body_bytes"`
}

type ProxyConfig struct {
	DefaultModel string            `mapstructure:"default_model"`
	ModelMap     map[string]string `mapstructure:"model_map"`
}

func DefaultAuthFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "opencode", "auth.json")
}

func New(configFile string) (*viper.Viper, Config, error) {
	v := viper.New()
	v.SetDefault("auth_file", DefaultAuthFile())
	v.SetDefault("base_url", ZenBase)
	v.SetDefault("log_level", "info")
	v.SetDefault("log_format", "text")
	v.SetDefault("server.listen", "127.0.0.1:8090")
	v.SetDefault("server.max_body_bytes", int64(16<<20))
	v.SetDefault("proxy.default_model", "claude-sonnet-4-6")
	v.SetDefault("proxy.model_map", map[string]string{})
	v.SetEnvPrefix("OPENCODE_PROXY")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	_ = v.BindEnv("server.api_key", "OPENCODE_PROXY_API_KEY")
	_ = v.BindEnv("api_key", "OPENCODE_API_KEY")

	if configFile != "" {
		v.SetConfigFile(configFile)
	} else {
		home, _ := os.UserHomeDir()
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(filepath.Join(home, ".config", "opencode-proxy"))
	}
	if err := v.ReadInConfig(); err != nil {
		if configFile != "" {
			return nil, Config{}, fmt.Errorf("read config: %w", err)
		}
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, Config{}, fmt.Errorf("read config: %w", err)
		}
	}
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, Config{}, fmt.Errorf("decode config: %w", err)
	}
	return v, cfg, nil
}

// ResolveAPIKey returns the upstream OpenCode Zen API key and where it came
// from. Priority: explicit config/env key, then OpenCode's own auth store.
func (c Config) ResolveAPIKey() (string, string, error) {
	if key := strings.TrimSpace(c.APIKey); key != "" {
		return key, "config", nil
	}
	key, err := readOpenCodeAuth(c.AuthFile)
	if err != nil {
		return "", "", err
	}
	if key == "" {
		return "", "", fmt.Errorf("no OpenCode API key found; set OPENCODE_API_KEY or run `opencode auth login` for the opencode provider")
	}
	return key, "opencode_auth", nil
}

// readOpenCodeAuth reads an API key from an opencode auth.json file. The file
// maps provider IDs to credentials; both "opencode" and "zen" are accepted.
func readOpenCodeAuth(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errorsIsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read auth file: %w", err)
	}
	var parsed map[string]struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("parse auth file: %w", err)
	}
	for _, provider := range []string{"opencode", "zen"} {
		if entry, ok := parsed[provider]; ok && entry.Key != "" {
			return entry.Key, nil
		}
	}
	return "", nil
}

func errorsIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}

// ResolveModel maps a client-requested model to an upstream model.
//
// Order: exact model_map entry, then a known Zen model (with any trailing
// -YYYYMMDD date suffix stripped), then the configured default model. When
// known models are unavailable the request passes through unchanged so an
// unreachable catalog never rewrites an explicit choice.
func (c Config) ResolveModel(requested string, knownModels []string) string {
	if mapped := c.Proxy.ModelMap[requested]; mapped != "" {
		return mapped
	}
	canonical := dateSuffix.ReplaceAllString(requested, "")
	for _, model := range knownModels {
		if model == requested || model == canonical {
			return model
		}
	}
	if knownModels == nil {
		return requested
	}
	return c.Proxy.DefaultModel
}
