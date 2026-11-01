package execstream

import (
	"testing"
	"time"
)

// SetHeartbeatInterval changes the heartbeat interval for the test t.
func SetHeartbeatInterval(t *testing.T, d time.Duration) {
	old := heartbeatInterval
	heartbeatInterval = d
	t.Cleanup(func() { heartbeatInterval = old })
}

var RoundMs = roundMs
