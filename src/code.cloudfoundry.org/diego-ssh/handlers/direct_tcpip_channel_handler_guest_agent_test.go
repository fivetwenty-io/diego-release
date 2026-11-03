//go:build !windows && !windows2012R2

package handlers_test

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"path/filepath"
	"sync"

	"code.cloudfoundry.org/diego-ssh/daemon"
	"code.cloudfoundry.org/diego-ssh/handlers"
	"code.cloudfoundry.org/diego-ssh/test_helpers"
	"code.cloudfoundry.org/lager/v3/lagertest"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/crypto/ssh"
)

var _ = Describe("DirectTcpipChannelHandler with a guest agent", func() {
	var (
		client             *ssh.Client
		agent              *handlers.GuestAgent
		echoAddress        string
		echoConns          *int
		echoMu             sync.Mutex
		connectionFinished chan struct{}
	)

	// echoRelay echoes each forward's bytes back from the guest.
	echoRelay := func(line string, conn net.Conn) {
		defer conn.Close()
		data, err := io.ReadAll(conn)
		if err != nil {
			return
		}
		_, _ = conn.Write(data)
	}

	BeforeEach(func() {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(listener.Close)
		echoAddress = listener.Addr().String()
		count := new(int)
		echoMu.Lock()
		echoConns = count
		echoMu.Unlock()
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				echoMu.Lock()
				*count++
				echoMu.Unlock()
				go func() {
					defer conn.Close()
					_, _ = io.Copy(conn, conn)
				}()
			}
		}()
		agent = nil
	})

	JustBeforeEach(func() {
		serverSSHConfig := &ssh.ServerConfig{NoClientAuth: true}
		serverSSHConfig.AddHostKey(TestHostKey)
		handler := handlers.NewDirectTcpipChannelHandler(handlers.NewGuestAgentDialer(agent, &net.Dialer{}))
		sshd := daemon.New(lagertest.NewTestLogger("test"), serverSSHConfig, nil, map[string]handlers.NewChannelHandler{"direct-tcpip": handler})

		serverNetConn, clientNetConn := test_helpers.Pipe()
		connectionFinished = make(chan struct{})
		go func() {
			sshd.HandleConnection(serverNetConn)
			close(connectionFinished)
		}()
		client = test_helpers.NewClient(clientNetConn, nil)
	})

	AfterEach(func() {
		client.Close()
		Eventually(connectionFinished).Should(BeClosed())
	})

	expectDirectEcho := func() {
		conn, err := client.Dial("tcp", echoAddress)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()
		_, err = conn.Write([]byte("Hello, World!\n"))
		Expect(err).NotTo(HaveOccurred())
		data, err := bufio.NewReader(conn).ReadString('\n')
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(Equal("Hello, World!\n"))
		echoMu.Lock()
		defer echoMu.Unlock()
		Expect(*echoConns).To(Equal(1))
	}

	Context("when the app runs in a VM whose guest has an agent", func() {
		var relay *fakeGuestAgentRelay

		BeforeEach(func() {
			relay = startFakeGuestAgentRelay(func(line string) string {
				if line == "forward 127.0.0.1 9" {
					return "error: connecting to 127.0.0.1 port 9 in the guest: connection refused"
				}
				return agentOn(line)
			}, echoRelay)
			agent = guestAgentAt(relay.path)
		})

		It("forwards through the guest agent's relay with the target host and port", func() {
			conn, err := client.Dial("tcp", "127.0.0.1:8080")
			Expect(err).NotTo(HaveOccurred())
			conn.Close()
			Expect(relay.Lines()).To(Equal([]string{"ping", "forward 127.0.0.1 8080"}))
		})

		It("passes megabytes of arbitrary bytes both ways unchanged, and the client's half-close to the guest", func() {
			conn, err := client.Dial("tcp", "localhost:8080")
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			data := make([]byte, 4<<20)
			_, err = rand.Read(data)
			Expect(err).NotTo(HaveOccurred())

			go func() {
				defer GinkgoRecover()
				_, err := conn.Write(data)
				Expect(err).NotTo(HaveOccurred())
				Expect(conn.(interface{ CloseWrite() error }).CloseWrite()).To(Succeed())
			}()

			got, err := io.ReadAll(conn)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(got)).To(Equal(len(data)))
			Expect(bytes.Equal(got, data)).To(BeTrue())
		})

		It("rejects the channel with the relay's reason when the guest cannot connect", func() {
			_, err := client.Dial("tcp", "127.0.0.1:9")
			Expect(err).To(Equal(&ssh.OpenChannelError{
				Reason:  ssh.ConnectionFailed,
				Message: "connecting to 127.0.0.1 port 9 in the guest: connection refused",
			}))
		})
	})

	Context("when the guest's agent is down", func() {
		var relay *fakeGuestAgentRelay

		BeforeEach(func() {
			relay = startFakeGuestAgentRelay(relayAnswers("down"), echoRelay)
			agent = guestAgentAt(relay.path)
		})

		It("dials the target from the container, as before, so that ports the hypervisor forwards still work", func() {
			expectDirectEcho()
			Expect(relay.Lines()).To(Equal([]string{"ping"}))
		})
	})

	Context("when a process of a runC or gVisor app answers ok at the socket", func() {
		var relay *fakeGuestAgentRelay

		BeforeEach(func() {
			relay = startFakeGuestAgentRelay(agentOn, echoRelay)
			agent = handlers.NewGuestAgent(relay.path, writeMountInfo(false))
		})

		It("dials the target directly and never tells that process the target", func() {
			expectDirectEcho()
			Expect(relay.Lines()).To(BeEmpty())
		})
	})

	Context("when the container has no guest agent socket", func() {
		BeforeEach(func() {
			agent = guestAgentAt(filepath.Join(GinkgoT().TempDir(), "vm-launch-agent.sock"))
		})

		It("dials the target directly and copies data, as before", func() {
			expectDirectEcho()
		})
	})

	Context("when there is no guest agent at all", func() {
		It("dials the target directly and copies data, as before", func() {
			expectDirectEcho()
		})
	})
})
