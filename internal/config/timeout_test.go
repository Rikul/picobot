package config

import (
	"testing"
	"time"
)

func TestAgentTimeout(t *testing.T) {
	tests := []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{"default for zero", 0, time.Duration(DefaultAgentTimeoutS) * time.Second},
		{"default for negative", -1, time.Duration(DefaultAgentTimeoutS) * time.Second},
		{"explicit value", 45, 45 * time.Second},
		{"five minutes", 300, 5 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentTimeout(tc.seconds); got != tc.want {
				t.Errorf("AgentTimeout(%d) = %s, want %s", tc.seconds, got, tc.want)
			}
		})
	}
}

func TestApplyEnvOverrides_AgentTimeoutS(t *testing.T) {
	cfg := DefaultConfig()
	t.Setenv("PICOBOT_AGENT_TIMEOUT_S", "120")
	applyEnvOverrides(&cfg)
	if cfg.Agents.Defaults.AgentTimeoutS != 120 {
		t.Errorf("AgentTimeoutS = %d, want 120", cfg.Agents.Defaults.AgentTimeoutS)
	}
}

func TestApplyEnvOverrides_AgentTimeoutS_IgnoresInvalid(t *testing.T) {
	cfg := DefaultConfig()
	want := cfg.Agents.Defaults.AgentTimeoutS
	t.Setenv("PICOBOT_AGENT_TIMEOUT_S", "nope")
	applyEnvOverrides(&cfg)
	if cfg.Agents.Defaults.AgentTimeoutS != want {
		t.Errorf("AgentTimeoutS = %d, want unchanged %d", cfg.Agents.Defaults.AgentTimeoutS, want)
	}
}
