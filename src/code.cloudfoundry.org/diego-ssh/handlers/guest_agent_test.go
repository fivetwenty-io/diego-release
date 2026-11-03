//go:build !windows && !windows2012R2

package handlers_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"code.cloudfoundry.org/diego-ssh/handlers"
	"code.cloudfoundry.org/diego-ssh/handlers/fakes"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeGuestAgentRelay stands in for vm-launch's guest agent relay. It reads
// each client's first line, records it, and writes the answer that answer
// gives. After an ok to any line but a ping, stream, when set, gets the
// client's connection. Every other connection closes after the answer, as
// the real relay's does. The contract spec checks it against
// testdata/guest-agent-relay-contract.txt, which vm-launch's specs check
// the real relay against.
type fakeGuestAgentRelay struct {
	path     string
	listener net.Listener

	answer func(line string) string
	stream func(line string, conn net.Conn)

	mu    sync.Mutex
	lines []string
}

func startFakeGuestAgentRelay(answer func(line string) string, stream func(line string, conn net.Conn)) *fakeGuestAgentRelay {
	// A short directory keeps the socket's path within the limit.
	dir, err := os.MkdirTemp("", "relay")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(os.RemoveAll, dir)

	relay := &fakeGuestAgentRelay{path: filepath.Join(dir, "agent.sock"), answer: answer, stream: stream}
	relay.listener, err = net.Listen("unix", relay.path)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(relay.listener.Close)

	go func() {
		for {
			conn, err := relay.listener.Accept()
			if err != nil {
				return
			}
			go relay.handle(conn)
		}
	}()

	return relay
}

func (relay *fakeGuestAgentRelay) handle(conn net.Conn) {
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return
	}
	line = line[:len(line)-1]

	relay.mu.Lock()
	relay.lines = append(relay.lines, line)
	relay.mu.Unlock()

	answer := relay.answer(line)
	_, err = io.WriteString(conn, answer+"\n")
	if err != nil || answer != "ok" || line == "ping" || relay.stream == nil {
		conn.Close()
		return
	}

	relay.stream(line, &bufferedConn{Conn: conn, reader: reader})
}

func (relay *fakeGuestAgentRelay) Lines() []string {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return append([]string{}, relay.lines...)
}

// bufferedConn reads what the relay's line reader has buffered first, and
// still half-closes.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (conn *bufferedConn) Read(p []byte) (int, error) {
	return conn.reader.Read(p)
}

func (conn *bufferedConn) CloseWrite() error {
	return conn.Conn.(*net.UnixConn).CloseWrite()
}

// relayAnswers answers each line as vm-launch's relay does for a guest in
// state, which is "agent" for a guest whose agent answers, "off" for one
// without an agent, or "down" for one whose agent cannot be reached.
func relayAnswers(state string) func(string) string {
	return func(line string) string {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 1 && fields[0] == "session":
		case len(fields) == 3 && fields[0] == "forward":
			if _, err := strconv.Atoi(fields[2]); err != nil {
				return fmt.Sprintf("error: %q is not a port", fields[2])
			}
		case len(fields) == 1 && fields[0] == "ping":
		default:
			return fmt.Sprintf(`error: the guest agent relay takes "session", "forward HOST PORT", or "ping", not %q`, line)
		}
		switch state {
		case "off":
			if fields[0] == "ping" {
				return "off: this guest has no agent for cf ssh, because it is a spec"
			}
			return "error: this guest has no agent for cf ssh, because it is a spec"
		case "down":
			if fields[0] == "ping" {
				return "error: the guest agent did not answer a ping: the agent is down"
			}
			return "error: the guest agent did not answer within 42s: the agent is down"
		}
		return "ok"
	}
}

// agentOn answers as a relay whose guest's agent answers.
var agentOn = relayAnswers("agent")

// guestAgentAt returns the gate for the relay at socketPath, as sshd builds
// it when the executor passes -vmContainer.
func guestAgentAt(socketPath string) *handlers.GuestAgent {
	return handlers.NewGuestAgent(socketPath)
}

// stalledRelay listens at a socket and never answers, and returns the
// socket's path.
func stalledRelay() string {
	dir, err := os.MkdirTemp("", "relay")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(os.RemoveAll, dir)
	path := filepath.Join(dir, "agent.sock")
	listener, err := net.Listen("unix", path)
	Expect(err).NotTo(HaveOccurred())
	var mu sync.Mutex
	var conns []net.Conn
	DeferCleanup(func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	return path
}

// heldPings answers as relayAnswers(state) does, but holds each answer to a
// ping until release closes, so that a spec can start a burst of requests
// while the first ping is still in flight.
func heldPings(state string, release <-chan struct{}) func(string) string {
	answer := relayAnswers(state)
	return func(line string) string {
		if line == "ping" {
			<-release
		}
		return answer(line)
	}
}

// burst runs n calls of f at once and waits for them all.
func burst(n int, f func()) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			f()
		}()
	}
	wg.Wait()
}

// countLines returns how many of lines are line.
func countLines(lines []string, line string) int {
	n := 0
	for _, l := range lines {
		if l == line {
			n++
		}
	}
	return n
}

var _ = Describe("GuestAgent", func() {
	expectState := func(agent *handlers.GuestAgent, want handlers.GuestAgentState) error {
		GinkgoHelper()
		state, err := agent.Check()
		Expect(state).To(Equal(want))
		return err
	}

	It("is absent without a socket, as before the guest has started", func() {
		agent := guestAgentAt(filepath.Join(GinkgoT().TempDir(), "agent.sock"))
		Expect(expectState(agent, handlers.GuestAgentAbsent)).To(Succeed())
	})

	It("is absent when the path is not a socket, or is a link to one", func() {
		path := filepath.Join(GinkgoT().TempDir(), "agent.sock")
		Expect(os.WriteFile(path, nil, 0600)).To(Succeed())
		Expect(expectState(guestAgentAt(path), handlers.GuestAgentAbsent)).To(Succeed())

		relay := startFakeGuestAgentRelay(agentOn, nil)
		link := filepath.Join(GinkgoT().TempDir(), "agent.sock")
		Expect(os.Symlink(relay.path, link)).To(Succeed())
		Expect(expectState(guestAgentAt(link), handlers.GuestAgentAbsent)).To(Succeed())
		Expect(relay.Lines()).To(BeEmpty())
	})

	It("is always absent when nil, as it is without -vmContainer", func() {
		var agent *handlers.GuestAgent
		Expect(expectState(agent, handlers.GuestAgentAbsent)).To(Succeed())
	})

	It("is absent, with the relay's reason, when the relay says the guest has no agent by configuration", func() {
		relay := startFakeGuestAgentRelay(relayAnswers("off"), nil)
		err := expectState(guestAgentAt(relay.path), handlers.GuestAgentAbsent)
		Expect(err).To(MatchError("this guest has no agent for cf ssh, because it is a spec"))
		Expect(relay.Lines()).To(Equal([]string{"ping"}))
	})

	It("is down, with the relay's reason, when the relay says the guest's agent did not answer", func() {
		relay := startFakeGuestAgentRelay(relayAnswers("down"), nil)
		err := expectState(guestAgentAt(relay.path), handlers.GuestAgentDown)
		Expect(err).To(MatchError("the guest agent did not answer a ping: the agent is down"))
		Expect(relay.Lines()).To(Equal([]string{"ping"}))
	})

	It("is down when the relay does not know the ping, as an older vm-launch's does not", func() {
		relay := startFakeGuestAgentRelay(func(line string) string {
			return `error: the guest agent relay takes "session" or "forward HOST PORT", not "` + line + `"`
		}, nil)
		Expect(expectState(guestAgentAt(relay.path), handlers.GuestAgentDown)).To(HaveOccurred())
	})

	It("is down when the relay closes without an answer", func() {
		dir, err := os.MkdirTemp("", "relay")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		path := filepath.Join(dir, "agent.sock")
		listener, err := net.Listen("unix", path)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(listener.Close)
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				conn.Close()
			}
		}()
		Expect(expectState(guestAgentAt(path), handlers.GuestAgentDown)).To(HaveOccurred())
	})

	It("gives up on a relay that does not answer within one timeout of four seconds, for the dial and the answer together", func() {
		agent := guestAgentAt(stalledRelay())
		start := time.Now()
		Expect(expectState(agent, handlers.GuestAgentDown)).To(HaveOccurred())
		Expect(time.Since(start)).To(BeNumerically("~", 4*time.Second, 750*time.Millisecond))
	})

	It("is ready once the relay answers the ping with ok, and does not ask again for ten seconds", func() {
		relay := startFakeGuestAgentRelay(agentOn, nil)
		agent := guestAgentAt(relay.path)
		now := time.Now()
		var mu sync.Mutex
		handlers.SetGuestAgentClock(agent, func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		})
		advance := func(d time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(d)
		}

		Expect(expectState(agent, handlers.GuestAgentReady)).To(Succeed())
		advance(9 * time.Second)
		Expect(expectState(agent, handlers.GuestAgentReady)).To(Succeed())
		Expect(relay.Lines()).To(Equal([]string{"ping"}))

		advance(2 * time.Second)
		Expect(expectState(agent, handlers.GuestAgentReady)).To(Succeed())
		Expect(relay.Lines()).To(Equal([]string{"ping", "ping"}))
	})

	It("is down once the ok has lapsed and the relay says the agent is down", func() {
		var mu sync.Mutex
		state := "agent"
		relay := startFakeGuestAgentRelay(func(line string) string {
			mu.Lock()
			defer mu.Unlock()
			return relayAnswers(state)(line)
		}, nil)
		agent := guestAgentAt(relay.path)
		now := time.Now()
		handlers.SetGuestAgentClock(agent, func() time.Time { return now })

		Expect(expectState(agent, handlers.GuestAgentReady)).To(Succeed())
		mu.Lock()
		state = "down"
		mu.Unlock()
		Expect(expectState(agent, handlers.GuestAgentReady)).To(Succeed())
		now = now.Add(11 * time.Second)
		Expect(expectState(agent, handlers.GuestAgentDown)).To(HaveOccurred())
		Expect(relay.Lines()).To(Equal([]string{"ping", "ping"}))
	})

	It("asks again after an answer other than ok, because the relay may still be starting", func() {
		var on bool
		var mu sync.Mutex
		relay := startFakeGuestAgentRelay(func(string) string {
			mu.Lock()
			defer mu.Unlock()
			if on {
				return "ok"
			}
			return "error: not yet"
		}, nil)
		agent := guestAgentAt(relay.path)
		Expect(expectState(agent, handlers.GuestAgentDown)).To(MatchError("not yet"))
		mu.Lock()
		on = true
		mu.Unlock()
		Expect(expectState(agent, handlers.GuestAgentReady)).To(Succeed())
		Expect(relay.Lines()).To(Equal([]string{"ping", "ping"}))
	})

	DescribeTable("sends one ping for a burst of 20 checks while no ok holds, and gives each the ping's result",
		func(state string, want handlers.GuestAgentState) {
			release := make(chan struct{})
			relay := startFakeGuestAgentRelay(heldPings(state, release), nil)
			agent := guestAgentAt(relay.path)

			results := make(chan handlers.GuestAgentState, 20)
			done := make(chan struct{})
			go func() {
				defer close(done)
				burst(20, func() {
					state, _ := agent.Check()
					results <- state
				})
			}()
			Eventually(relay.Lines).Should(Equal([]string{"ping"}))
			Consistently(relay.Lines, 300*time.Millisecond).Should(Equal([]string{"ping"}))
			close(release)
			Eventually(done).Should(BeClosed())

			close(results)
			for state := range results {
				Expect(state).To(Equal(want))
			}
			Expect(relay.Lines()).To(Equal([]string{"ping"}))
		},
		Entry("when the agent answers", "agent", handlers.GuestAgentReady),
		Entry("when the agent is down", "down", handlers.GuestAgentDown),
		Entry("when the guest has no agent", "off", handlers.GuestAgentAbsent),
	)

	Describe("Forward", func() {
		It("asks the relay for the host and port, and returns the stream that follows the ok", func() {
			relay := startFakeGuestAgentRelay(agentOn, func(line string, conn net.Conn) {
				defer conn.Close()
				_, _ = io.WriteString(conn, "from the guest")
			})
			conn, err := guestAgentAt(relay.path).Forward("localhost", 8080)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()
			Expect(io.ReadAll(conn)).To(Equal([]byte("from the guest")))
			Expect(relay.Lines()).To(Equal([]string{"forward localhost 8080"}))
		})

		It("connects to the guest's own address when the host is empty", func() {
			relay := startFakeGuestAgentRelay(agentOn, func(line string, conn net.Conn) { conn.Close() })
			conn, err := guestAgentAt(relay.path).Forward("", 8080)
			Expect(err).NotTo(HaveOccurred())
			conn.Close()
			Expect(relay.Lines()).To(Equal([]string{"forward 127.0.0.1 8080"}))
		})

		It("returns the relay's reason when the forward cannot connect, and pings again before the next one", func() {
			relay := startFakeGuestAgentRelay(func(line string) string {
				if line == "forward 127.0.0.1 9" {
					return "error: connecting to 127.0.0.1 port 9 in the guest: connection refused"
				}
				return "ok"
			}, nil)
			agent := guestAgentAt(relay.path)
			Expect(expectState(agent, handlers.GuestAgentReady)).To(Succeed())
			_, err := agent.Forward("127.0.0.1", 9)
			Expect(err).To(MatchError("connecting to 127.0.0.1 port 9 in the guest: connection refused"))
			Expect(expectState(agent, handlers.GuestAgentReady)).To(Succeed())
			Expect(relay.Lines()).To(Equal([]string{"ping", "forward 127.0.0.1 9", "ping"}))
		})

		It("refuses a host that would not be one field of the relay's line, and a port that is not a TCP port, without asking the relay", func() {
			relay := startFakeGuestAgentRelay(agentOn, nil)
			agent := guestAgentAt(relay.path)
			for _, host := range []string{"localhost 80\nforward evil", "local\thost", "local host", "local\x00host"} {
				_, err := agent.Forward(host, 8080)
				Expect(err).To(MatchError(ContainSubstring("is not a host name or address")))
			}
			_, err := agent.Forward("localhost", 0)
			Expect(err).To(MatchError("the target port 0 is not a TCP port"))
			_, err = agent.Forward("localhost", 65536)
			Expect(err).To(MatchError("the target port 65536 is not a TCP port"))
			Expect(relay.Lines()).To(BeEmpty())
		})
	})

	Describe("the dialer", func() {
		var fallback *fakes.FakeDialer

		BeforeEach(func() {
			fallback = &fakes.FakeDialer{}
			fallback.DialReturns(nil, errors.New("the fallback dialer"))
		})

		It("dials with the fallback dialer, unchanged, when there is no relay socket", func() {
			dialer := handlers.NewGuestAgentDialer(guestAgentAt(filepath.Join(GinkgoT().TempDir(), "agent.sock")), fallback)
			_, err := dialer.Dial("tcp", "10.0.0.1:8080")
			Expect(err).To(MatchError("the fallback dialer"))
			Expect(fallback.DialCallCount()).To(Equal(1))
			network, addr := fallback.DialArgsForCall(0)
			Expect(network).To(Equal("tcp"))
			Expect(addr).To(Equal("10.0.0.1:8080"))
		})

		DescribeTable("dials with the fallback dialer, after one ping, when the agent is not ready",
			func(state string) {
				relay := startFakeGuestAgentRelay(relayAnswers(state), nil)
				dialer := handlers.NewGuestAgentDialer(guestAgentAt(relay.path), fallback)
				_, err := dialer.Dial("tcp", "127.0.0.1:8080")
				Expect(err).To(MatchError("the fallback dialer"))
				Expect(fallback.DialCallCount()).To(Equal(1))
				Expect(relay.Lines()).To(Equal([]string{"ping"}))
			},
			Entry("when the guest has no agent by configuration", "off"),
			Entry("when the guest's agent is down, so a declared port still reaches the guest through the hypervisor", "down"),
		)

		It("dials with the fallback dialer when it has no gate, as without -vmContainer", func() {
			dialer := handlers.NewGuestAgentDialer(nil, fallback)
			_, err := dialer.Dial("tcp", "prod-db.internal:5432")
			Expect(err).To(MatchError("the fallback dialer"))
			Expect(fallback.DialCallCount()).To(Equal(1))
		})

		It("forwards through the guest agent when it is ready, with the host and port of the address", func() {
			relay := startFakeGuestAgentRelay(agentOn, func(line string, conn net.Conn) { conn.Close() })
			dialer := handlers.NewGuestAgentDialer(guestAgentAt(relay.path), fallback)
			for _, addr := range []string{"localhost:8080", "::1:8081", "[::1]:8082"} {
				conn, err := dialer.Dial("tcp", addr)
				Expect(err).NotTo(HaveOccurred())
				conn.Close()
			}
			Expect(relay.Lines()).To(Equal([]string{"ping", "forward localhost 8080", "forward ::1 8081", "forward ::1 8082"}))
			Expect(fallback.DialCallCount()).To(BeZero())
		})

		It("sends one ping for a burst of 20 forwards, and forwards each one", func() {
			release := make(chan struct{})
			relay := startFakeGuestAgentRelay(heldPings("agent", release), func(line string, conn net.Conn) { conn.Close() })
			dialer := handlers.NewGuestAgentDialer(guestAgentAt(relay.path), fallback)

			done := make(chan struct{})
			go func() {
				defer close(done)
				burst(20, func() {
					conn, err := dialer.Dial("tcp", "127.0.0.1:8080")
					Expect(err).NotTo(HaveOccurred())
					conn.Close()
				})
			}()
			Eventually(relay.Lines).Should(Equal([]string{"ping"}))
			Consistently(relay.Lines, 300*time.Millisecond).Should(Equal([]string{"ping"}))
			close(release)
			Eventually(done).Should(BeClosed())

			lines := relay.Lines()
			Expect(lines).To(HaveLen(21))
			Expect(lines[0]).To(Equal("ping"))
			Expect(countLines(lines, "forward 127.0.0.1 8080")).To(Equal(20))
			Expect(fallback.DialCallCount()).To(BeZero())
		})

		It("refuses an address without a port", func() {
			relay := startFakeGuestAgentRelay(agentOn, nil)
			dialer := handlers.NewGuestAgentDialer(guestAgentAt(relay.path), fallback)
			_, err := dialer.Dial("tcp", "localhost")
			Expect(err).To(MatchError(`the target address "localhost" has no port`))
			_, err = dialer.Dial("tcp", "localhost:http")
			Expect(err).To(MatchError(`the target address "localhost:http" has no port`))
		})

		It("keeps the guest's reason when a forward fails and the agent still answers", func() {
			relay := startFakeGuestAgentRelay(func(line string) string {
				if line == "ping" {
					return "ok"
				}
				return "error: connecting to 127.0.0.1 port 9 in the guest: connection refused"
			}, nil)
			dialer := handlers.NewGuestAgentDialer(guestAgentAt(relay.path), fallback)
			_, err := dialer.Dial("tcp", "127.0.0.1:9")
			Expect(err).To(MatchError("connecting to 127.0.0.1 port 9 in the guest: connection refused"))
			Expect(fallback.DialCallCount()).To(BeZero())
			Expect(relay.Lines()).To(Equal([]string{"ping", "forward 127.0.0.1 9", "ping"}))
		})

		It("dials with the fallback dialer when a forward fails because the agent has gone down", func() {
			var mu sync.Mutex
			state := "agent"
			relay := startFakeGuestAgentRelay(func(line string) string {
				mu.Lock()
				defer mu.Unlock()
				answer := relayAnswers(state)(line)
				if line == "ping" {
					// The agent goes down just after it answers.
					state = "down"
				}
				return answer
			}, nil)
			dialer := handlers.NewGuestAgentDialer(guestAgentAt(relay.path), fallback)
			_, err := dialer.Dial("tcp", "127.0.0.1:8080")
			Expect(err).To(MatchError("the fallback dialer"))
			Expect(fallback.DialCallCount()).To(Equal(1))
			_, addr := fallback.DialArgsForCall(0)
			Expect(addr).To(Equal("127.0.0.1:8080"))
			Expect(relay.Lines()).To(Equal([]string{"ping", "forward 127.0.0.1 8080", "ping"}))

			_, err = dialer.Dial("tcp", "127.0.0.1:8080")
			Expect(err).To(MatchError("the fallback dialer"))
			Expect(fallback.DialCallCount()).To(Equal(2))
			Expect(relay.Lines()).To(Equal([]string{"ping", "forward 127.0.0.1 8080", "ping", "ping"}))
		})
	})

	It("has a fake relay that keeps the contract of vm-launch's relay", func() {
		data, err := os.ReadFile(filepath.Join("testdata", "guest-agent-relay-contract.txt"))
		Expect(err).NotTo(HaveOccurred())
		echo := func(line string, conn net.Conn) {
			defer conn.Close()
			_, _ = io.Copy(conn, conn)
			_ = conn.(interface{ CloseWrite() error }).CloseWrite()
		}
		relays := map[string]*fakeGuestAgentRelay{}
		for _, state := range []string{"agent", "off", "down"} {
			relays[state] = startFakeGuestAgentRelay(relayAnswers(state), echo)
		}

		cases := 0
		for _, text := range strings.Split(string(data), "\n") {
			if text == "" || strings.HasPrefix(text, "#") {
				continue
			}
			fields := strings.Split(text, " | ")
			Expect(fields).To(HaveLen(4), text)
			state, line, want, after := fields[0], strings.ReplaceAll(fields[1], "PORT", "8080"), fields[2], fields[3]
			Expect(relays).To(HaveKey(state), text)

			conn, err := net.Dial("unix", relays[state].path)
			Expect(err).NotTo(HaveOccurred())
			Expect(conn.SetDeadline(time.Now().Add(10 * time.Second))).To(Succeed())
			_, err = io.WriteString(conn, line+"\n")
			Expect(err).NotTo(HaveOccurred())
			reader := bufio.NewReader(conn)
			answer, err := reader.ReadString('\n')
			Expect(err).NotTo(HaveOccurred())
			answer = strings.TrimSuffix(answer, "\n")
			switch want {
			case "ok":
				Expect(answer).To(Equal("ok"), text)
			case "error:":
				Expect(answer).To(MatchRegexp(`^error: \S`), text)
			case "off:":
				Expect(answer).To(MatchRegexp(`^off: \S`), text)
			default:
				Fail("the contract has no answer " + want)
			}
			switch after {
			case "eof":
				_, err := reader.ReadByte()
				Expect(err).To(MatchError(io.EOF), text)
			case "stream":
				_, err := io.WriteString(conn, "through the guest")
				Expect(err).NotTo(HaveOccurred())
				Expect(conn.(*net.UnixConn).CloseWrite()).To(Succeed())
				Expect(io.ReadAll(reader)).To(Equal([]byte("through the guest")), text)
			default:
				Fail("the contract has no after " + after)
			}
			conn.Close()
			cases++
		}
		Expect(cases).To(Equal(8))
	})
})
