//go:build !windows && !windows2012R2

package handlers

import (
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

// GuestAgentSocketPath is where Garden's vm-launch listens for the guest
// agent's clients in the container of an app that runs in a VM. The shell
// that sshd starts in such a container is vm-launch's shim, which runs each
// command in the VM's guest through this socket.
const GuestAgentSocketPath = "/tmp/vm-launch-agent.sock"

const (
	// guestAgentPingTimeout bounds a whole ping, from the dial to the
	// relay's answer. The relay answers once the guest's agent has answered
	// it over vsock, and gives up on the agent within three seconds.
	guestAgentPingTimeout = 4 * time.Second

	// guestAgentAnswerLifetime is how long the relay's ok to a ping holds.
	// Within it, scp, sftp, and forwards go to the guest without another
	// ping. After it, or after a forward fails, the next one pings again,
	// so an agent that has gone down is noticed within this time.
	guestAgentAnswerLifetime = 10 * time.Second

	// guestAgentForwardTimeout bounds the relay's answer to a forward,
	// which comes once the guest has connected to the target. The relay
	// waits up to 45 seconds for a guest that is still booting.
	guestAgentForwardTimeout = 60 * time.Second

	// guestAgentMaxAnswer is the most bytes the relay's answer may have.
	guestAgentMaxAnswer = 512

	// guestAgentOffPrefix starts the relay's answer to a ping when its
	// guest has no agent by configuration, such as a vm-qemu container
	// without vsock.
	guestAgentOffPrefix = "off: "
)

// GuestAgentState is what a GuestAgent found when it was asked whether sshd
// may serve through the guest agent.
type GuestAgentState int

const (
	// GuestAgentAbsent means the container has no guest agent: there is no
	// relay socket, or the relay says that its guest has no agent by
	// configuration. sshd serves scp, sftp, and forwards in the container,
	// as it did before the agent existed.
	GuestAgentAbsent GuestAgentState = iota

	// GuestAgentReady means the relay answered a ping with ok, which it
	// does only once the guest's agent has answered it.
	GuestAgentReady

	// GuestAgentDown means the relay is there but its agent did not answer
	// a ping. The app's files are in the guest, so sshd refuses scp and
	// sftp rather than serve the container's files.
	GuestAgentDown
)

// GuestAgent decides whether sshd serves scp, sftp, and direct-tcpip
// forwards through a VM guest's agent, because the app's files and ports
// are in the guest rather than in the container.
//
// sshd builds one only when the executor has passed it -vmContainer, which
// the executor adds to the SSH daemon's arguments in the main action of a VM
// container and nowhere else. In such a container the app runs in the guest
// and only the platform runs in the container, so the relay socket is
// vm-launch's. In a runC or gVisor container, any process of the app could
// create a socket at GuestAgentSocketPath, which is why sshd never builds a
// GuestAgent there.
type GuestAgent struct {
	socketPath string

	// okUntil is when the relay's last ok to a ping stops holding, in
	// nanoseconds since the epoch, or 0 when none holds.
	okUntil atomic.Int64
	now     func() time.Time

	// probe is the ping in flight, which every caller that finds no ok
	// holding waits for instead of sending its own.
	probeMu sync.Mutex
	probe   *guestAgentProbe
}

// guestAgentProbe is one ping and its result, which is set before done
// closes.
type guestAgentProbe struct {
	done  chan struct{}
	state GuestAgentState
	err   error
}

// NewGuestAgent returns the gate for the relay at socketPath. sshd passes
// GuestAgentSocketPath.
func NewGuestAgent(socketPath string) *GuestAgent {
	return &GuestAgent{socketPath: socketPath, now: time.Now}
}

// Check reports whether sshd should serve through the guest agent, and for
// GuestAgentDown, and for GuestAgentAbsent when the relay said why, the
// relay's reason. A nil GuestAgent is always absent. When no ok holds,
// concurrent calls share one ping, so a burst of requests sends the relay
// one ping rather than one each.
func (agent *GuestAgent) Check() (GuestAgentState, error) {
	if agent == nil {
		return GuestAgentAbsent, nil
	}
	if agent.answerHolds() {
		return GuestAgentReady, nil
	}

	// vm-launch creates the socket itself, so a link is never the relay.
	info, err := os.Lstat(agent.socketPath)
	if err != nil || info.Mode().Type() != os.ModeSocket {
		return GuestAgentAbsent, nil
	}

	agent.probeMu.Lock()
	if agent.answerHolds() {
		agent.probeMu.Unlock()
		return GuestAgentReady, nil
	}
	if probe := agent.probe; probe != nil {
		agent.probeMu.Unlock()
		<-probe.done
		return probe.state, probe.err
	}
	probe := &guestAgentProbe{done: make(chan struct{})}
	agent.probe = probe
	agent.probeMu.Unlock()

	probe.state, probe.err = agent.ping()
	if probe.state == GuestAgentReady {
		agent.okUntil.Store(agent.now().Add(guestAgentAnswerLifetime).UnixNano())
	}

	agent.probeMu.Lock()
	agent.probe = nil
	agent.probeMu.Unlock()
	close(probe.done)
	return probe.state, probe.err
}

func (agent *GuestAgent) answerHolds() bool {
	return agent.now().UnixNano() < agent.okUntil.Load()
}

// ping asks the relay whether its agent answers.
func (agent *GuestAgent) ping() (GuestAgentState, error) {
	conn, err := agent.ask("ping", guestAgentPingTimeout)
	if err == nil {
		_ = conn.Close()
		return GuestAgentReady, nil
	}
	var off guestAgentOffError
	if errors.As(err, &off) {
		return GuestAgentAbsent, err
	}
	return GuestAgentDown, err
}

// guestAgentOffError is the relay's answer when its guest has no agent by
// configuration.
type guestAgentOffError struct{ reason string }

func (e guestAgentOffError) Error() string { return e.reason }

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
		if reason, off := strings.CutPrefix(answer, guestAgentOffPrefix); off {
			err = guestAgentOffError{reason: reason}
		} else {
			err = errors.New(strings.TrimPrefix(answer, "error: "))
		}
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
// agent when it is ready, so that a forward reaches the app's ports in the
// guest, and through fallback otherwise. A guest without an agent gets the
// container's dial that it always had. While the agent is down, the dialer
// also dials through fallback, so that a forward still reaches the ports
// that the hypervisor forwards from the container into the guest, and a
// forward to any other port in the container is refused there, as it is
// without the agent. When the relay fails a forward and the agent then no
// longer answers a ping, the dialer falls back in the same way.
func NewGuestAgentDialer(agent *GuestAgent, fallback Dialer) Dialer {
	return &guestAgentDialer{agent: agent, fallback: fallback}
}

func (dialer *guestAgentDialer) Dial(network, addr string) (net.Conn, error) {
	if network != "tcp" {
		return dialer.fallback.Dial(network, addr)
	}
	if state, _ := dialer.agent.Check(); state != GuestAgentReady {
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
	if state, _ := dialer.agent.Check(); state == GuestAgentReady {
		return nil, err
	}
	return dialer.fallback.Dial(network, addr)
}
