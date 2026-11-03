//go:build windows || windows2012R2

package main

import (
	"net"
	"time"

	"code.cloudfoundry.org/diego-ssh/handlers"
)

// newChannelHandlers returns the session and direct-tcpip handlers. A
// Windows cell runs no VM apps, so it has no guest agent gate and ignores
// vmContainer.
func newChannelHandlers(runner handlers.Runner, shellLocator handlers.ShellLocator, defaultEnv map[string]string, keepalive time.Duration, _ bool) (handlers.NewChannelHandler, handlers.NewChannelHandler) {
	return handlers.NewSessionChannelHandler(runner, shellLocator, defaultEnv, keepalive),
		handlers.NewDirectTcpipChannelHandler(&net.Dialer{})
}
