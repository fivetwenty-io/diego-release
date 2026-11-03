//go:build !windows && !windows2012R2

package main_test

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"code.cloudfoundry.org/diego-ssh/cmd/sshd/testrunner"
	"github.com/tedsuo/ifrit"
	ginkgomon "github.com/tedsuo/ifrit/ginkgomon_v2"
	"golang.org/x/crypto/ssh"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("SSH daemon with a guest agent", func() {
	var (
		workDir string
		port    uint16
		process ifrit.Process
		client  *ssh.Client

		relayMu    sync.Mutex
		relayLines []string

		vmContainer bool
		extraArgs   []string
		echoAddress string
	)

	// startRelay listens where the specs' sshd looks for the guest agent's
	// relay, answers every line with ok, closes after a ping, and echoes a
	// forward's bytes.
	startRelay := func() {
		listener, err := net.Listen("unix", filepath.Join(workDir, guestAgentDir, "vm-launch-agent.sock"))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(listener.Close)
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					reader := bufio.NewReader(conn)
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					relayMu.Lock()
					relayLines = append(relayLines, strings.TrimSuffix(line, "\n"))
					relayMu.Unlock()
					if _, err := io.WriteString(conn, "ok\n"); err != nil || line == "ping\n" {
						return
					}
					data, _ := io.ReadAll(reader)
					_, _ = conn.Write(data)
				}()
			}
		}()
	}

	lines := func() []string {
		relayMu.Lock()
		defer relayMu.Unlock()
		return append([]string{}, relayLines...)
	}

	BeforeEach(func() {
		var err error
		// A short directory keeps the relay socket's path within the limit.
		workDir, err = os.MkdirTemp("", "sshd")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, workDir)
		Expect(os.Mkdir(filepath.Join(workDir, guestAgentDir), 0700)).To(Succeed())
		relayMu.Lock()
		relayLines = nil
		relayMu.Unlock()

		port, err = portAllocator.ClaimPorts(1)
		Expect(err).NotTo(HaveOccurred())

		vmContainer = false
		extraArgs = nil

		// echoAddress is a TCP port in the container, which a forward that
		// sshd dials itself reaches.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(listener.Close)
		echoAddress = listener.Addr().String()
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					_, _ = io.Copy(conn, conn)
				}()
			}
		}()
	})

	JustBeforeEach(func() {
		runner := testrunner.New(sshdPath, testrunner.Args{
			Address:       "127.0.0.1:" + strconv.Itoa(int(port)),
			HostKey:       hostKeyPem,
			AuthorizedKey: publicAuthorizedKey,
			VMContainer:   vmContainer,
			ExtraArgs:     extraArgs,
		})
		runner.Command.Dir = workDir
		process = ginkgomon.Invoke(runner)

		key, err := ssh.ParsePrivateKey([]byte(privateKeyPem))
		Expect(err).NotTo(HaveOccurred())
		client, err = ssh.Dial("tcp", "127.0.0.1:"+strconv.Itoa(int(port)), &ssh.ClientConfig{
			User:            os.Getenv("USER"),
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if client != nil {
			client.Close()
		}
		ginkgomon.Kill(process, 3*time.Second)
	})

	// expectEcho forwards to addr through sshd and expects what it writes
	// back.
	expectEcho := func(addr, text string) {
		conn, err := client.Dial("tcp", addr)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()
		_, err = io.WriteString(conn, text)
		Expect(err).NotTo(HaveOccurred())
		Expect(conn.(interface{ CloseWrite() error }).CloseWrite()).To(Succeed())
		Expect(io.ReadAll(conn)).To(Equal([]byte(text)))
	}

	Context("when the executor appends the vm-container marker, as it does in the main action of a VM container", func() {
		BeforeEach(func() {
			vmContainer = true
		})

		Context("and the relay answers", func() {
			BeforeEach(func() {
				startRelay()
			})

			It("sends scp and forwards through the one gate that main wires, which pings once for both", func() {
				session, err := client.NewSession()
				Expect(err).NotTo(HaveOccurred())
				// The shell runs scp, as the shim would in the guest, and
				// what it answers does not matter here.
				_, _ = session.CombinedOutput("scp -h")
				session.Close()
				Expect(lines()).To(Equal([]string{"ping"}))

				expectEcho("127.0.0.1:8080", "through the guest")
				Expect(lines()).To(Equal([]string{"ping", "forward 127.0.0.1 8080"}))
			})
		})

		Context("and there is no relay socket", func() {
			It("dials the target itself, as before", func() {
				expectEcho(echoAddress, "from the container")
				Expect(lines()).To(BeEmpty())
			})
		})
	})

	Context("when the executor appends no marker, though a socket at the relay's path answers ok, as an app in a runC or gVisor container could make one", func() {
		BeforeEach(func() {
			startRelay()
		})

		It("serves scp and forwards itself and never asks the socket", func() {
			session, err := client.NewSession()
			Expect(err).NotTo(HaveOccurred())
			_, _ = session.CombinedOutput("scp -h")
			session.Close()

			expectEcho(echoAddress, "from the container")
			Expect(lines()).To(BeEmpty())
		})
	})

	Context("when another positional argument comes before the vm-container marker, so that two follow the flags", func() {
		BeforeEach(func() {
			startRelay()
			vmContainer = true
			extraArgs = []string{"extra"}
		})

		It("treats the session as outside a VM, serves scp and forwards itself, and never asks the socket", func() {
			session, err := client.NewSession()
			Expect(err).NotTo(HaveOccurred())
			_, _ = session.CombinedOutput("scp -h")
			session.Close()

			expectEcho(echoAddress, "from the container")
			Expect(lines()).To(BeEmpty())
		})
	})

	Context("when the executor passes the bare -vmContainer flag, as an executor from before the marker did", func() {
		BeforeEach(func() {
			startRelay()
			extraArgs = []string{"-vmContainer"}
		})

		// The flag is still defined, so the daemon starts and honors it,
		// which marks the container as a VM container without the marker.
		It("starts and sends scp and forwards through the guest agent", func() {
			session, err := client.NewSession()
			Expect(err).NotTo(HaveOccurred())
			_, _ = session.CombinedOutput("scp -h")
			session.Close()
			Expect(lines()).To(Equal([]string{"ping"}))

			expectEcho("127.0.0.1:8080", "through the guest")
			Expect(lines()).To(Equal([]string{"ping", "forward 127.0.0.1 8080"}))
		})
	})
})
