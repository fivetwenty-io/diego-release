//go:build !windows && !windows2012R2

package handlers

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	// GuestAgentSocketPath is where Garden's vm-launch listens for the
	// guest agent's clients in the container of an app that runs in a VM.
	// The shell that sshd starts in such a container is vm-launch's shim,
	// which runs each command in the VM's guest through this socket.
	GuestAgentSocketPath = "/tmp/vm-launch-agent.sock"

	// GuestAgentShellPath is the shell over which Garden binds vm-launch in
	// the container of an app whose VM guest has the agent. Garden makes
	// that mount when it creates the container, and no process in a
	// container can add a mount to sshd's mount namespace, so the mount is
	// what tells sshd that it runs in such a container. A socket alone does
	// not, because any process of a runC or gVisor app can create one at
	// GuestAgentSocketPath.
	GuestAgentShellPath = "/bin/bash"

	// GuestAgentMountInfoPath is the mount table of sshd's own mount
	// namespace.
	GuestAgentMountInfoPath = "/proc/self/mountinfo"
)

const (
	// guestAgentPingTimeout bounds a whole ping, from the dial to the
	// relay's answer. The relay answers once the guest's agent has answered
	// it over vsock, and gives up on the agent within three seconds.
	guestAgentPingTimeout = 4 * time.Second

	// guestAgentAnswerLifetime is how long the relay's ok to a ping holds.
	// Within it, scp, sftp, and forwards go to the guest without another
	// ping. After it, or after a forward fails, the next one pings again,
	// so an agent that has gone down sends sshd back to the container
	// within this time.
	guestAgentAnswerLifetime = 10 * time.Second

	// guestAgentForwardTimeout bounds the relay's answer to a forward,
	// which comes once the guest has connected to the target. The relay
	// waits up to 45 seconds for a guest that is still booting.
	guestAgentForwardTimeout = 60 * time.Second

	// guestAgentMaxAnswer is the most bytes the relay's answer may have.
	guestAgentMaxAnswer = 512
)

// GuestAgent decides whether sshd serves scp, sftp, and direct-tcpip
// forwards through a VM guest's agent, because the app's files and ports
// are in the guest rather than in the container. It does so only when
// sshd's mount table shows the shell that Garden binds for the agent, the
// socket is there, and the relay answers a ping with ok, which it does only
// once the guest's agent has answered it. On anything else sshd serves them
// in the container as it always has.
type GuestAgent struct {
	socketPath    string
	mountInfoPath string

	// shellBound says whether the mount table shows GuestAgentShellPath as
	// a mount point. Garden makes its mounts before sshd starts, so the
	// mount table is read once.
	shellBoundOnce sync.Once
	shellBound     bool

	// okUntil is when the relay's last ok to a ping stops holding, in
	// nanoseconds since the epoch, or 0 when none holds.
	okUntil atomic.Int64
	now     func() time.Time
}

// NewGuestAgent returns the gate for the relay at socketPath, which opens
// only when the mount table at mountInfoPath shows GuestAgentShellPath as
// a mount point. sshd passes GuestAgentSocketPath and
// GuestAgentMountInfoPath.
func NewGuestAgent(socketPath, mountInfoPath string) *GuestAgent {
	return &GuestAgent{socketPath: socketPath, mountInfoPath: mountInfoPath, now: time.Now}
}

// Available reports whether sshd should serve through the guest agent. A
// nil GuestAgent is never available.
func (agent *GuestAgent) Available() bool {
	if agent == nil || !agent.isShellBound() {
		return false
	}
	if agent.now().UnixNano() < agent.okUntil.Load() {
		return true
	}

	// vm-launch creates the socket itself, so a link is never the relay.
	info, err := os.Lstat(agent.socketPath)
	if err != nil || info.Mode().Type() != os.ModeSocket {
		return false
	}

	conn, err := agent.ask("ping", guestAgentPingTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()

	agent.okUntil.Store(agent.now().Add(guestAgentAnswerLifetime).UnixNano())
	return true
}

// isShellBound reports whether the mount table shows GuestAgentShellPath
// as a mount point. A mount table that cannot be read shows none.
func (agent *GuestAgent) isShellBound() bool {
	agent.shellBoundOnce.Do(func() {
		file, err := os.Open(agent.mountInfoPath)
		if err != nil {
			return
		}
		defer file.Close()

		// The mount point is the fifth field. It is escaped, with \040
		// for a space, but GuestAgentShellPath has nothing to escape.
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 4096), 1<<20)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) > 4 && fields[4] == GuestAgentShellPath {
				agent.shellBound = true
				return
			}
		}
	})
	return agent.shellBound
}

// Forward connects to host and port in the guest through the relay and
// returns a connection that carries the raw bytes of the guest's
// connection, with a half-close in each direction.
func (agent *GuestAgent) Forward(host string, port int) (net.Conn, error) {
	line, err := forwardLine(host, port)
	if err != nil {
		return nil, err
	}
	return agent.forward(line)
}

func (agent *GuestAgent) forward(line string) (net.Conn, error) {
	conn, err := agent.ask(line, guestAgentForwardTimeout)
	if err != nil {
		// The agent may have gone down, so the next request pings again.
		agent.okUntil.Store(0)
		return nil, err
	}
	return conn, nil
}

// forwardLine returns the relay's line for a forward to host and port. An
// empty host is the guest's own address, as it is to the agent.
func forwardLine(host string, port int) (string, error) {
	if host == "" {
		host = "127.0.0.1"
	}
	// The relay splits its line at white space, so a host must be one
	// field of it, and the line must end where the relay expects.
	if strings.IndexFunc(host, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", fmt.Errorf("the target host %q is not a host name or address", host)
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("the target port %d is not a TCP port", port)
	}
	return fmt.Sprintf("forward %s %d", host, port), nil
}

// ask connects to the relay, writes line, and reads the relay's one-line
// answer, all within timeout. On ok it returns the connection, positioned
// just past the answer, with no deadline.
func (agent *GuestAgent) ask(line string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	dialer := net.Dialer{Deadline: deadline}
	conn, err := dialer.Dial("unix", agent.socketPath)
	if err != nil {
		return nil, err
	}

	err = conn.SetDeadline(deadline)
	if err == nil {
		_, err = io.WriteString(conn, line+"\n")
	}
	var answer string
	if err == nil {
		answer, err = readAnswer(conn)
	}
	if err == nil && answer != "ok" {
		err = errors.New(strings.TrimPrefix(answer, "error: "))
	}
	if err == nil {
		err = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	return conn, nil
}

// readAnswer reads one line one byte at a time, so that it never takes any
// of the stream that follows an ok.
func readAnswer(r io.Reader) (string, error) {
	var line []byte
	b := make([]byte, 1)
	for len(line) < guestAgentMaxAnswer {
		_, err := io.ReadFull(r, b)
		if err != nil {
			return "", fmt.Errorf("reading the guest agent relay's answer: %w", err)
		}
		if b[0] == '\n' {
			return string(line), nil
		}
		line = append(line, b[0])
	}

	return "", errors.New("the guest agent relay's answer is too long")
}

type guestAgentDialer struct {
	agent    *GuestAgent
	fallback Dialer
}

// NewGuestAgentDialer returns a Dialer that connects through the guest
// agent when it is available, so that a forward reaches the app's ports in
// the guest, and through fallback otherwise. When the relay fails a
// forward and the agent then no longer answers a ping, it dials through
// fallback, so that a forward still reaches the ports that the hypervisor
// forwards from the container while the agent is down.
func NewGuestAgentDialer(agent *GuestAgent, fallback Dialer) Dialer {
	return &guestAgentDialer{agent: agent, fallback: fallback}
}

func (dialer *guestAgentDialer) Dial(network, addr string) (net.Conn, error) {
	if network != "tcp" || !dialer.agent.Available() {
		return dialer.fallback.Dial(network, addr)
	}

	// The direct-tcpip handler joins the host and port with a colon and no
	// brackets, so the port is whatever follows the last one.
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return nil, fmt.Errorf("the target address %q has no port", addr)
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return nil, fmt.Errorf("the target address %q has no port", addr)
	}
	host := strings.TrimSuffix(strings.TrimPrefix(addr[:i], "["), "]")
	line, err := forwardLine(host, port)
	if err != nil {
		return nil, err
	}

	conn, err := dialer.agent.forward(line)
	if err == nil {
		return conn, nil
	}
	// An agent that still answers gave the guest's own reason, such as a
	// refused connect, which stands.
	if dialer.agent.Available() {
		return nil, err
	}
	return dialer.fallback.Dial(network, addr)
}
