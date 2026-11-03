//go:build !windows && !windows2012R2

package main

import (
	"net"
	"time"

	"code.cloudfoundry.org/diego-ssh/handlers"
)

// guestAgentSocketPath and guestAgentMountInfoPath are where sshd looks for
// the guest agent. They are variables only so that the sshd suite can build
// a binary that looks in a directory of its own, with -ldflags -X.
var (
	guestAgentSocketPath    = handlers.GuestAgentSocketPath
	guestAgentMountInfoPath = handlers.GuestAgentMountInfoPath
)

// newChannelHandlers returns the session and direct-tcpip handlers, which
// share one guest agent gate. In the container of an app whose VM guest has
// the agent, scp, sftp, and forwards then reach the app's files and ports
// in the guest.
func newChannelHandlers(runner handlers.Runner, shellLocator handlers.ShellLocator, defaultEnv map[string]string, keepalive time.Duration) (handlers.NewChannelHandler, handlers.NewChannelHandler) {
	agent := handlers.NewGuestAgent(guestAgentSocketPath, guestAgentMountInfoPath)
	return handlers.NewSessionChannelHandlerWithGuestAgent(runner, shellLocator, defaultEnv, keepalive, agent),
		handlers.NewDirectTcpipChannelHandler(handlers.NewGuestAgentDialer(agent, &net.Dialer{}))
}
