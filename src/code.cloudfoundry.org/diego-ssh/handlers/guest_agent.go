package handlers

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

// GuestAgentSocketPath is where Garden's vm-launch listens for the guest
// agent's clients in the container of an app that runs in a VM. The shell
// that sshd starts in such a container is vm-launch's shim, which runs each
// command in the VM's guest through this socket. Containers of other apps
// have no socket at this path.
const GuestAgentSocketPath = "/tmp/vm-launch-agent.sock"

const (
	// guestAgentPingTimeout bounds the relay's answer to a ping, which it
	// gives without waiting for the guest.
	guestAgentPingTimeout = 2 * time.Second

	// guestAgentForwardTimeout bounds the relay's answer to a forward,
	// which comes once the guest has connected to the target. The relay
	// waits up to 45 seconds for a guest that is still booting.
	guestAgentForwardTimeout = 60 * time.Second

	// guestAgentMaxAnswer is the most bytes the relay's answer may have.
	guestAgentMaxAnswer = 512
)

// GuestAgent decides whether sshd serves scp, sftp, and direct-tcpip
// forwards through a VM guest's agent, because the app's files and ports
// are in the guest rather than in the container. It does so only when the
// socket exists and the relay answers a ping with ok. On anything else sshd
// serves them in the container as it always has.
type GuestAgent struct {
	socketPath string
	// on is set once the relay has answered a ping with ok. Whether the
	// guest has an agent does not change after the container starts, so a
	// true answer is kept for the life of the process. Any other answer is
	// asked again next time, because the relay may still be starting.
	on atomic.Bool
}

func NewGuestAgent(socketPath string) *GuestAgent {
	return &GuestAgent{socketPath: socketPath}
}

// Available reports whether the guest agent's relay is there and says the
// guest has an agent. A nil GuestAgent is never available.
func (agent *GuestAgent) Available() bool {
	if agent == nil {
		return false
	}
	if agent.on.Load() {
		return true
	}

	info, err := os.Stat(agent.socketPath)
	if err != nil || info.Mode().Type() != os.ModeSocket {
		return false
	}

	conn, err := agent.ask("ping", guestAgentPingTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()

	agent.on.Store(true)
	return true
}

// Forward connects to host and port in the guest through the relay and
// returns a connection that carries the raw bytes of the guest's
// connection, with a half-close in each direction.
func (agent *GuestAgent) Forward(host string, port int) (net.Conn, error) {
	if host == "" {
		host = "127.0.0.1"
	}
	// The relay splits its line at white space, so a host must be one
	// field of it, and the line must end where the relay expects.
	if strings.IndexFunc(host, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return nil, fmt.Errorf("the target host %q is not a host name or address", host)
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("the target port %d is not a TCP port", port)
	}

	return agent.ask(fmt.Sprintf("forward %s %d", host, port), guestAgentForwardTimeout)
}

// ask connects to the relay, writes line, and reads the relay's one-line
// answer. On ok it returns the connection, positioned just past the answer.
func (agent *GuestAgent) ask(line string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", agent.socketPath, timeout)
	if err != nil {
		return nil, err
	}

	err = conn.SetDeadline(time.Now().Add(timeout))
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
// the guest, and through fallback otherwise.
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

	return dialer.agent.Forward(host, port)
}
