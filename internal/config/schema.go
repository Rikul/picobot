package config

// Config holds picobot configuration (minimal for v0).
type Config struct {
	Agents    AgentsConfig    `json:"agents"`
	Channels  ChannelsConfig  `json:"channels"`
	Providers ProvidersConfig `json:"providers"`
}

type AgentsConfig struct {
	Defaults AgentDefaults `json:"defaults"`
}

type AgentDefaults struct {
	Workspace          string  `json:"workspace"`
	Model              string  `json:"model"`
	MaxTokens          int     `json:"maxTokens"`
	Temperature        float64 `json:"temperature"`
	MaxToolIterations  int     `json:"maxToolIterations"`
	HeartbeatIntervalS int     `json:"heartbeatIntervalS"`
	RequestTimeoutS    int     `json:"requestTimeoutS"`
}

type ChannelsConfig struct {
	Telegram TelegramConfig `json:"telegram"`
	Discord  DiscordConfig  `json:"discord"`
	WhatsApp WhatsAppConfig `json:"whatsapp"`
}

type DiscordConfig struct {
	Enabled   bool     `json:"enabled"`
	Token     string   `json:"token"`
	AllowFrom []string `json:"allowFrom"`
}

type TelegramConfig struct {
	Enabled   bool     `json:"enabled"`
	Token     string   `json:"token"`
	AllowFrom []string `json:"allowFrom"`
}

type WhatsAppConfig struct {
	Enabled   bool     `json:"enabled"`
	DBPath    string   `json:"dbPath"`
	AllowFrom []string `json:"allowFrom"`
}

type ProvidersConfig struct {
	OpenAI        *ProviderConfig      `json:"openai,omitempty"`
	Transcription *TranscriptionConfig `json:"transcription,omitempty"`
}

type ProviderConfig struct {
	APIKey  string `json:"apiKey"`
	APIBase string `json:"apiBase"`
}

// TranscriptionConfig configures speech-to-text for Telegram voice messages.
// Two modes are supported:
//
//   - CLI mode (recommended, no API key needed): set Command to the whisper
//     executable (e.g. "whisper" after `pip install openai-whisper`).
//     Model is the whisper model size: "tiny", "base", "small", "medium", "large".
//
//   - HTTP mode: set APIBase to a Whisper-compatible REST endpoint.
//     APIKey and Model ("whisper-1") are passed to that endpoint.
//
// CLI mode takes priority when Command is non-empty.
type TranscriptionConfig struct {
	// CLI mode
	Command string `json:"command,omitempty"` // e.g. "whisper" or "python3 -m whisper"
	Model   string `json:"model,omitempty"`   // whisper model size, default "tiny" for CLI

	// HTTP mode
	APIBase string `json:"apiBase,omitempty"`
	APIKey  string `json:"apiKey,omitempty"`
}
