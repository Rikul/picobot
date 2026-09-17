package config

import "time"

// DefaultAgentTimeoutS is the wall-clock timeout for a single agent turn
// when agentTimeoutS is missing or non-positive.
const DefaultAgentTimeoutS = 300

// AgentTimeout converts a configured timeout in seconds to a duration.
// Zero and negative values fall back to DefaultAgentTimeoutS (5 minutes).
func AgentTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = DefaultAgentTimeoutS
	}
	return time.Duration(seconds) * time.Second
}
