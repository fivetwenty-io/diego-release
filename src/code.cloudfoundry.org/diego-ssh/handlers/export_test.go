//go:build !windows && !windows2012R2

package handlers

import "time"

// SetGuestAgentClock makes agent read the time from now, so that specs can
// pass the lifetime of the relay's answer without waiting for it.
func SetGuestAgentClock(agent *GuestAgent, now func() time.Time) {
	agent.now = now
}
