// Package config loads environment settings and the apps.yaml source of truth.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Env holds process configuration from environment variables (see .env.example).
type Env struct {
	DiscordBotToken string
	DiscordAppID    string
	DiscordGuildID  string

	DeployHookSecret string
	HTTPAddr         string

	ASCKeyID          string
	ASCIssuerID       string
	ASCPrivateKeyPath string
	ASCVendorNumber   string

	GPlaySAJSONPath    string
	GPlayReportsBucket string

	GitHubToken string

	LLMBaseURL string
	LLMAPIKey  string
	LLMModel   string

	DBPath   string
	AppsYAML string
	TZ       string
	LogLevel slog.Level
}

// LoadEnv reads the environment. Only the Discord credentials are mandatory;
// every other integration is switched off when its credentials are missing.
func LoadEnv() (Env, error) {
	e := Env{
		DiscordBotToken:    os.Getenv("DISCORD_BOT_TOKEN"),
		DiscordAppID:       os.Getenv("DISCORD_APP_ID"),
		DiscordGuildID:     os.Getenv("DISCORD_GUILD_ID"),
		DeployHookSecret:   os.Getenv("DEPLOY_HOOK_SECRET"),
		HTTPAddr:           getenv("HTTP_ADDR", ":8080"),
		ASCKeyID:           os.Getenv("ASC_KEY_ID"),
		ASCIssuerID:        os.Getenv("ASC_ISSUER_ID"),
		ASCPrivateKeyPath:  os.Getenv("ASC_PRIVATE_KEY_PATH"),
		ASCVendorNumber:    os.Getenv("ASC_VENDOR_NUMBER"),
		GPlaySAJSONPath:    os.Getenv("GPLAY_SA_JSON_PATH"),
		GPlayReportsBucket: os.Getenv("GPLAY_REPORTS_BUCKET"),
		GitHubToken:        os.Getenv("GITHUB_TOKEN"),
		LLMBaseURL:         getenv("LLM_BASE_URL", "http://spark:4000/v1"),
		LLMAPIKey:          os.Getenv("LLM_API_KEY"),
		LLMModel:           os.Getenv("LLM_MODEL"),
		DBPath:             getenv("DB_PATH", "/data/ol1nbot.db"),
		AppsYAML:           getenv("APPS_YAML", "/config/apps.yaml"),
		TZ:                 getenv("TZ", "Europe/Prague"),
	}
	if err := e.LogLevel.UnmarshalText([]byte(getenv("LOG_LEVEL", "info"))); err != nil {
		return e, fmt.Errorf("LOG_LEVEL: %w", err)
	}
	var missing []string
	for name, v := range map[string]string{
		"DISCORD_BOT_TOKEN": e.DiscordBotToken,
		"DISCORD_APP_ID":    e.DiscordAppID,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return e, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	return e, nil
}

// ASCEnabled reports whether App Store Connect API credentials are configured.
func (e Env) ASCEnabled() bool {
	return e.ASCKeyID != "" && e.ASCIssuerID != "" && e.ASCPrivateKeyPath != ""
}

// GPlayEnabled reports whether a Google Play service account is configured.
func (e Env) GPlayEnabled() bool { return e.GPlaySAJSONPath != "" }

// LLMEnabled reports whether a LiteLLM model is configured.
func (e Env) LLMEnabled() bool { return e.LLMBaseURL != "" && e.LLMModel != "" }

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
