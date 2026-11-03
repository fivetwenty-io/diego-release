package handlers_test

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"code.cloudfoundry.org/diego-ssh/handlers"
	"code.cloudfoundry.org/diego-ssh/handlers/fakes"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeGuestAgentRelay stands in for vm-launch's guest agent relay. It reads
// each client's first line, records it, and writes the answer that answer
// gives. After an ok, stream, when set, gets the client's connection.
type fakeGuestAgentRelay struct {
	path     string
	listener net.Listener

	answer func(line string) string
	stream func(line string, conn net.Conn)

	mu    sync.Mutex
	lines []string
}

func startFakeGuestAgentRelay(answer func(line string) string, stream func(line string, conn net.Conn)) *fakeGuestAgentRelay {
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
	if err != nil || answer != "ok" || relay.stream == nil {
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

// agentOn answers every line with ok, as a relay whose guest has an agent.
func agentOn(string) string { return "ok" }

var _ = Describe("GuestAgent", func() {
	BeforeEach(func() {
		if runtime.GOOS == "windows" {
			Skip("the guest agent runs only on Linux cells")
		}
	})

	It("is not available without a socket, as in the container of an app that does not run in a VM", func() {
		agent := handlers.NewGuestAgent(filepath.Join(GinkgoT().TempDir(), "agent.sock"))
		Expect(agent.Available()).To(BeFalse())
	})

	It("is not available when the path is not a socket", func() {
		path := filepath.Join(GinkgoT().TempDir(), "agent.sock")
		Expect(os.WriteFile(path, nil, 0600)).To(Succeed())
		Expect(handlers.NewGuestAgent(path).Available()).To(BeFalse())
	})

	It("is never available when nil", func() {
		var agent *handlers.GuestAgent
		Expect(agent.Available()).To(BeFalse())
	})

	It("is not available when the relay says the guest has no agent", func() {
		relay := startFakeGuestAgentRelay(func(string) string {
			return "error: this guest has no agent for cf ssh, because it is a spec"
		}, nil)
		Expect(handlers.NewGuestAgent(relay.path).Available()).To(BeFalse())
		Expect(relay.Lines()).To(Equal([]string{"ping"}))
	})

	It("is not available when the relay does not know the ping, as an older vm-launch's does not", func() {
		relay := startFakeGuestAgentRelay(func(line string) string {
			return `error: the guest agent relay takes "session" or "forward HOST PORT", not "` + line + `"`
		}, nil)
		Expect(handlers.NewGuestAgent(relay.path).Available()).To(BeFalse())
	})

	It("is not available when the relay closes without an answer, or answers too slowly", func() {
		dir, err := os.MkdirTemp("", "relay")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		path := filepath.Join(dir, "agent.sock")
		listener, err := net.Listen("unix", path)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(listener.Close)
		conns := make(chan net.Conn, 2)
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				conns <- conn
			}
		}()

		agent := handlers.NewGuestAgent(path)
		go func() {
			conn := <-conns
			conn.Close()
		}()
		Expect(agent.Available()).To(BeFalse())

		start := time.Now()
		Expect(agent.Available()).To(BeFalse())
		Expect(time.Since(start)).To(BeNumerically("<", 5*time.Second))
		(<-conns).Close()
	})

	It("is available once the relay answers the ping with ok, and does not ask again", func() {
		relay := startFakeGuestAgentRelay(agentOn, nil)
		agent := handlers.NewGuestAgent(relay.path)
		Expect(agent.Available()).To(BeTrue())
		Expect(agent.Available()).To(BeTrue())
		Expect(relay.Lines()).To(Equal([]string{"ping"}))
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
		agent := handlers.NewGuestAgent(relay.path)
		Expect(agent.Available()).To(BeFalse())
		mu.Lock()
		on = true
		mu.Unlock()
		Expect(agent.Available()).To(BeTrue())
		Expect(relay.Lines()).To(Equal([]string{"ping", "ping"}))
	})

	Describe("Forward", func() {
		It("asks the relay for the host and port, and returns the stream that follows the ok", func() {
			relay := startFakeGuestAgentRelay(agentOn, func(line string, conn net.Conn) {
				defer conn.Close()
				_, _ = io.WriteString(conn, "from the guest")
			})
			conn, err := handlers.NewGuestAgent(relay.path).Forward("localhost", 8080)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()
			Expect(io.ReadAll(conn)).To(Equal([]byte("from the guest")))
			Expect(relay.Lines()).To(Equal([]string{"forward localhost 8080"}))
		})

		It("connects to the guest's own address when the host is empty", func() {
			relay := startFakeGuestAgentRelay(agentOn, func(line string, conn net.Conn) { conn.Close() })
			conn, err := handlers.NewGuestAgent(relay.path).Forward("", 8080)
			Expect(err).NotTo(HaveOccurred())
			conn.Close()
			Expect(relay.Lines()).To(Equal([]string{"forward 127.0.0.1 8080"}))
		})

		It("returns the relay's reason when the forward cannot connect", func() {
			relay := startFakeGuestAgentRelay(func(string) string {
				return "error: connecting to 127.0.0.1 port 9 in the guest: connection refused"
			}, nil)
			_, err := handlers.NewGuestAgent(relay.path).Forward("127.0.0.1", 9)
			Expect(err).To(MatchError("connecting to 127.0.0.1 port 9 in the guest: connection refused"))
		})

		It("refuses a host that would not be one field of the relay's line, and a port that is not a TCP port, without asking the relay", func() {
			relay := startFakeGuestAgentRelay(agentOn, nil)
			agent := handlers.NewGuestAgent(relay.path)
			for _, host := range []string{"localhost 80\nforward evil", "local\thost", "local\u00a0host", "local\x00host"} {
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

		It("dials with the fallback dialer, unchanged, when the guest agent is not available", func() {
			dialer := handlers.NewGuestAgentDialer(handlers.NewGuestAgent(filepath.Join(GinkgoT().TempDir(), "agent.sock")), fallback)
			_, err := dialer.Dial("tcp", "10.0.0.1:8080")
			Expect(err).To(MatchError("the fallback dialer"))
			Expect(fallback.DialCallCount()).To(Equal(1))
			network, addr := fallback.DialArgsForCall(0)
			Expect(network).To(Equal("tcp"))
			Expect(addr).To(Equal("10.0.0.1:8080"))
		})

		It("forwards through the guest agent when it is available, with the host and port of the address", func() {
			relay := startFakeGuestAgentRelay(agentOn, func(line string, conn net.Conn) { conn.Close() })
			dialer := handlers.NewGuestAgentDialer(handlers.NewGuestAgent(relay.path), fallback)
			for _, addr := range []string{"localhost:8080", "::1:8081", "[::1]:8082"} {
				conn, err := dialer.Dial("tcp", addr)
				Expect(err).NotTo(HaveOccurred())
				conn.Close()
			}
			Expect(relay.Lines()).To(Equal([]string{"ping", "forward localhost 8080", "forward ::1 8081", "forward ::1 8082"}))
			Expect(fallback.DialCallCount()).To(BeZero())
		})

		It("refuses an address without a port", func() {
			relay := startFakeGuestAgentRelay(agentOn, nil)
			dialer := handlers.NewGuestAgentDialer(handlers.NewGuestAgent(relay.path), fallback)
			_, err := dialer.Dial("tcp", "localhost")
			Expect(err).To(MatchError(`the target address "localhost" has no port`))
			_, err = dialer.Dial("tcp", "localhost:http")
			Expect(err).To(MatchError(`the target address "localhost:http" has no port`))
		})
	})
})
