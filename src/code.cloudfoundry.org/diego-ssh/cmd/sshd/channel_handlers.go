//go:build !windows && !windows2012R2

package main

import (
	"net"
	"time"

	"code.cloudfoundry.org/diego-ssh/handlers"
)

// guestAgentSocketPath is where sshd looks for the guest agent's relay. It
// is a variable only so that the sshd suite can build a binary that looks
// in a directory of its own, with -ldflags -X.
var guestAgentSocketPath = handlers.GuestAgentSocketPath

// newChannelHandlers returns the session and direct-tcpip handlers. In a VM
// container, which the executor marks with a trailing vm-container argument, they share one
// guest agent gate, so that scp, sftp, and forwards reach the app's files and
// ports in the guest. Anywhere else they are the plain handlers, which never
// look at the relay's socket, because any process of a runC or gVisor app
// could create one there.
func newChannelHandlers(runner handlers.Runner, shellLocator handlers.ShellLocator, defaultEnv map[string]string, keepalive time.Duration, vmContainer bool) (handlers.NewChannelHandler, handlers.NewChannelHandler) {
	if !vmContainer {
		return handlers.NewSessionChannelHandler(runner, shellLocator, defaultEnv, keepalive),
			handlers.NewDirectTcpipChannelHandler(&net.Dialer{})
	}
	agent := handlers.NewGuestAgent(guestAgentSocketPath)
	return handlers.NewSessionChannelHandlerWithGuestAgent(runner, shellLocator, defaultEnv, keepalive, agent),
		handlers.NewDirectTcpipChannelHandler(handlers.NewGuestAgentDialer(agent, &net.Dialer{}))
}
