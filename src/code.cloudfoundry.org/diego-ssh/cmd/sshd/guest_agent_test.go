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

// The mount table of a container, and the line for the shell that Garden
// binds for the guest agent in the container of a VM app.
const (
	containerMountInfo = "1205 1146 0:331 / / rw,relatime - overlay overlay rw\n" +
		"1206 1205 0:333 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw\n"
	guestShellMount = "1210 1205 8:1 /var/vcap/data/garden/vm-launch /bin/bash ro,nosuid,nodev,relatime - ext4 /dev/sda1 rw\n"
)

var _ = Describe("SSH daemon with a guest agent", func() {
	var (
		workDir string
		port    uint16
		process ifrit.Process
		client  *ssh.Client

		relayMu    sync.Mutex
		relayLines []string
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

	writeMountTable := func(contents string) {
		Expect(os.WriteFile(filepath.Join(workDir, guestAgentDir, "mountinfo"), []byte(contents), 0600)).To(Succeed())
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
	})

	JustBeforeEach(func() {
		runner := testrunner.New(sshdPath, testrunner.Args{
			Address:       "127.0.0.1:" + strconv.Itoa(int(port)),
			HostKey:       hostKeyPem,
			AuthorizedKey: publicAuthorizedKey,
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

	Context("when the mount table shows the shell that Garden binds for the agent", func() {
		BeforeEach(func() {
			writeMountTable(containerMountInfo + guestShellMount)
			startRelay()
		})

		It("sends scp and forwards through the one gate that main wires, which pings once for both", func() {
			session, err := client.NewSession()
			Expect(err).NotTo(HaveOccurred())
			// The shell runs scp, as the shim would in the guest, and what
			// it answers does not matter here.
			_, _ = session.CombinedOutput("scp -h")
			session.Close()
			Expect(lines()).To(Equal([]string{"ping"}))

			conn, err := client.Dial("tcp", "127.0.0.1:8080")
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()
			_, err = io.WriteString(conn, "through the guest")
			Expect(err).NotTo(HaveOccurred())
			Expect(conn.(interface{ CloseWrite() error }).CloseWrite()).To(Succeed())
			Expect(io.ReadAll(conn)).To(Equal([]byte("through the guest")))
			Expect(lines()).To(Equal([]string{"ping", "forward 127.0.0.1 8080"}))
		})
	})

	Context("when a socket answers ok but the mount table does not show the shell's mount, as in a runC or gVisor container", func() {
		var echoAddress string

		BeforeEach(func() {
			writeMountTable(containerMountInfo)
			startRelay()

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

		It("dials the target itself and never asks the socket", func() {
			conn, err := client.Dial("tcp", echoAddress)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()
			_, err = io.WriteString(conn, "from the container")
			Expect(err).NotTo(HaveOccurred())
			Expect(conn.(interface{ CloseWrite() error }).CloseWrite()).To(Succeed())
			Expect(io.ReadAll(conn)).To(Equal([]byte("from the container")))
			Expect(lines()).To(BeEmpty())
		})
	})
})
