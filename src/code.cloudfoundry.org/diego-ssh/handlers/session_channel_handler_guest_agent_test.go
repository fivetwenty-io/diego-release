//go:build !windows && !windows2012R2

package handlers_test

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"code.cloudfoundry.org/diego-ssh/daemon"
	"code.cloudfoundry.org/diego-ssh/handlers"
	"code.cloudfoundry.org/diego-ssh/handlers/fakes"
	"code.cloudfoundry.org/diego-ssh/test_helpers"
	"code.cloudfoundry.org/lager/v3/lagertest"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// testSFTPServerEnv makes the specs' own binary serve SFTP on its standard
// input and output, in place of a guest's sftp-server.
const testSFTPServerEnv = "DIEGO_SSH_HANDLERS_TEST_SFTP_SERVER"

func init() {
	if os.Getenv(testSFTPServerEnv) == "" {
		return
	}
	server, err := sftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{os.Stdin, os.Stdout})
	if err == nil {
		err = server.Serve()
	}
	if err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

var _ = Describe("SessionChannelHandler with a guest agent", func() {
	var (
		client             *ssh.Client
		runner             *fakes.FakeRunner
		shellLocator       *fakes.FakeShellLocator
		agent              *handlers.GuestAgent
		connectionFinished chan struct{}

		// shell stands in for vm-launch's shim. It records its arguments
		// in argsFile, starts this binary as the guest's sftp-server for
		// the sftp-server command, and runs anything else with /bin/sh,
		// as the guest does.
		shell, argsFile string
		binDir          string
	)

	BeforeEach(func() {
		dir := GinkgoT().TempDir()
		argsFile = filepath.Join(dir, "args")
		shell = filepath.Join(dir, "shim")
		testBinary, err := os.Executable()
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(shell, []byte(fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %s
case "$2" in
*sftp-server*) %s=1 exec %s ;;
esac
exec /bin/sh "$@"
`, argsFile, testSFTPServerEnv, testBinary)), 0755)).To(Succeed())

		// The fake scp copies its input to its output, says whether its
		// input is a terminal, and exits 3.
		binDir = filepath.Join(dir, "bin")
		Expect(os.Mkdir(binDir, 0755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(binDir, "scp"), []byte(`#!/bin/sh
if [ "$1" = "-tty" ]; then
	if [ -t 0 ]; then echo tty; else echo no-tty; fi
	exit 0
fi
cat
exit 3
`), 0755)).To(Succeed())

		runner = &fakes.FakeRunner{}
		realRunner := handlers.NewCommandRunner()
		runner.StartStub = realRunner.Start
		runner.WaitStub = realRunner.Wait
		runner.SignalStub = realRunner.Signal

		shellLocator = &fakes.FakeShellLocator{}
		shellLocator.ShellPathReturns(shell)

		agent = nil
	})

	JustBeforeEach(func() {
		serverSSHConfig := &ssh.ServerConfig{NoClientAuth: true}
		serverSSHConfig.AddHostKey(TestHostKey)

		handler := handlers.NewSessionChannelHandlerWithGuestAgent(runner, shellLocator, map[string]string{}, time.Second, agent)
		sshd := daemon.New(lagertest.NewTestLogger("test"), serverSSHConfig, nil, map[string]handlers.NewChannelHandler{"session": handler})

		serverNetConn, clientNetConn := test_helpers.Pipe()
		connectionFinished = make(chan struct{})
		go func() {
			sshd.HandleConnection(serverNetConn)
			close(connectionFinished)
		}()

		client = test_helpers.NewClient(clientNetConn, nil)
	})

	AfterEach(func() {
		Expect(client.Close()).To(Succeed())
		Eventually(connectionFinished).Should(BeClosed())
	})

	recordedArgs := func() []string {
		b, err := os.ReadFile(argsFile)
		Expect(err).NotTo(HaveOccurred())
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}

	newSession := func() *ssh.Session {
		session, err := client.NewSession()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = session.Close() })
		Expect(session.Setenv("PATH", binDir+":/bin:/usr/bin")).To(Succeed())
		return session
	}

	// scpUpload sends contents as name to an scp sink, as an scp client
	// does, and waits for each of the sink's confirmations.
	scpUpload := func(stdin io.Writer, stdout io.Reader, name string, contents []byte) {
		confirmation := make([]byte, 1)
		_, err := io.ReadFull(stdout, confirmation)
		Expect(err).NotTo(HaveOccurred())
		Expect(confirmation).To(Equal([]byte{0}))

		_, err = fmt.Fprintf(stdin, "C0644 %d %s\n", len(contents), name)
		Expect(err).NotTo(HaveOccurred())
		_, err = io.ReadFull(stdout, confirmation)
		Expect(err).NotTo(HaveOccurred())
		Expect(confirmation).To(Equal([]byte{0}))

		_, err = stdin.Write(append(contents, 0))
		Expect(err).NotTo(HaveOccurred())
		_, err = io.ReadFull(stdout, confirmation)
		Expect(err).NotTo(HaveOccurred())
		Expect(confirmation).To(Equal([]byte{0}))
	}

	Context("when the guest agent is available", func() {
		BeforeEach(func() {
			relay := startFakeGuestAgentRelay(agentOn, nil)
			agent = guestAgentAt(relay.path)
		})

		It("runs scp through the shell, which runs it in the guest, with its input, output, and exit status", func() {
			session := newSession()
			data := make([]byte, 4<<20)
			_, err := rand.Read(data)
			Expect(err).NotTo(HaveOccurred())
			session.Stdin = bytes.NewReader(data)
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			session.Stdout, session.Stderr = stdout, stderr

			err = session.Run("scp -v -t /target")
			Expect(err).To(BeAssignableToTypeOf(&ssh.ExitError{}))
			Expect(err.(*ssh.ExitError).ExitStatus()).To(Equal(3))

			Expect(stdout.Len()).To(Equal(len(data)))
			Expect(bytes.Equal(stdout.Bytes(), data)).To(BeTrue())
			Expect(stderr.String()).To(BeEmpty())
			Expect(recordedArgs()).To(Equal([]string{"-c", "scp -v -t /target"}))
			Expect(runner.StartCallCount()).To(Equal(1))
		})

		It("runs scp without a terminal, even when the client asked for one", func() {
			session := newSession()
			Expect(session.RequestPty("vt100", 43, 80, ssh.TerminalModes{})).To(Succeed())
			output, err := session.Output("scp -tty")
			Expect(err).NotTo(HaveOccurred())
			Expect(string(output)).To(Equal("no-tty\n"))
		})

		It("copies a file with the guest's own scp", func() {
			// A real scp sink confirms it is ready with a zero byte before it
			// reads anything.
			targetDir := GinkgoT().TempDir()
			probe, err := exec.Command("scp", "-t", targetDir).CombinedOutput()
			if len(probe) == 0 || probe[0] != 0 {
				Skip(fmt.Sprintf("this host's scp cannot run as a sink: %q, %v", probe, err))
			}
			contents := make([]byte, 1<<20)
			_, err = rand.Read(contents)
			Expect(err).NotTo(HaveOccurred())

			session, err := client.NewSession()
			Expect(err).NotTo(HaveOccurred())
			defer session.Close()
			stdin, err := session.StdinPipe()
			Expect(err).NotTo(HaveOccurred())
			stdout, err := session.StdoutPipe()
			Expect(err).NotTo(HaveOccurred())
			Expect(session.Start("scp -t " + targetDir)).To(Succeed())

			scpUpload(stdin, stdout, "binary.dat", contents)
			Expect(stdin.Close()).To(Succeed())
			Expect(session.Wait()).To(Succeed())

			Expect(os.ReadFile(filepath.Join(targetDir, "binary.dat"))).To(Equal(contents))
			Expect(recordedArgs()).To(Equal([]string{"-c", "scp -t " + targetDir}))
		})

		It("serves sftp with the guest's sftp-server through the shell", func() {
			targetDir := GinkgoT().TempDir()
			contents := make([]byte, 4<<20)
			_, err := rand.Read(contents)
			Expect(err).NotTo(HaveOccurred())

			sftpClient, err := sftp.NewClient(client)
			Expect(err).NotTo(HaveOccurred())
			defer sftpClient.Close()

			target := filepath.Join(targetDir, "binary.dat")
			file, err := sftpClient.Create(target)
			Expect(err).NotTo(HaveOccurred())
			_, err = file.Write(contents)
			Expect(err).NotTo(HaveOccurred())
			Expect(file.Close()).To(Succeed())
			Expect(os.ReadFile(target)).To(Equal(contents))

			file, err = sftpClient.Open(target)
			Expect(err).NotTo(HaveOccurred())
			read, err := io.ReadAll(file)
			Expect(err).NotTo(HaveOccurred())
			Expect(file.Close()).To(Succeed())
			Expect(bytes.Equal(read, contents)).To(BeTrue())

			args := recordedArgs()
			Expect(args).To(HaveLen(2))
			Expect(args[0]).To(Equal("-c"))
			Expect(args[1]).To(HavePrefix("for p in /usr/lib/openssh/sftp-server "))
		})

		It("says why sftp cannot work when the guest has no sftp-server", func() {
			for _, p := range []string{"/usr/lib/openssh/sftp-server", "/usr/libexec/openssh/sftp-server", "/usr/lib/ssh/sftp-server", "/usr/libexec/sftp-server", "/usr/lib/sftp-server"} {
				if _, err := os.Stat(p); err == nil {
					Skip("this host has an sftp-server at " + p)
				}
			}
			shellLocator.ShellPathReturns("/bin/sh")

			channel, requests, err := client.OpenChannel("session", nil)
			Expect(err).NotTo(HaveOccurred())
			defer channel.Close()
			accepted, err := channel.SendRequest("subsystem", true, ssh.Marshal(struct{ Subsystem string }{"sftp"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(accepted).To(BeTrue())

			stderr, err := io.ReadAll(channel.Stderr())
			Expect(err).NotTo(HaveOccurred())
			Expect(string(stderr)).To(Equal("sftp is not available, because this app's VM guest has no sftp-server\n"))

			var exitStatus *ssh.Request
			Eventually(requests).Should(Receive(&exitStatus))
			Expect(exitStatus.Type).To(Equal("exit-status"))
			Expect(exitStatus.Payload).To(Equal(ssh.Marshal(struct{ Status uint32 }{127})))
		})

		It("runs other commands through the shell as before", func() {
			session := newSession()
			output, err := session.Output("echo hello")
			Expect(err).NotTo(HaveOccurred())
			Expect(string(output)).To(Equal("hello\n"))
			Expect(recordedArgs()).To(Equal([]string{"-c", "echo hello"}))
		})
	})

	itServesTransfersInProcess := func() {
		It("copies with the in-process scp and never starts the shell", func() {
			targetDir := GinkgoT().TempDir()
			contents := []byte("---\nthis is a simple file\n\n")

			session, err := client.NewSession()
			Expect(err).NotTo(HaveOccurred())
			defer session.Close()
			stdin, err := session.StdinPipe()
			Expect(err).NotTo(HaveOccurred())
			stdout, err := session.StdoutPipe()
			Expect(err).NotTo(HaveOccurred())
			Expect(session.Start(fmt.Sprintf("scp -v -t %s", targetDir))).To(Succeed())

			scpUpload(stdin, stdout, "textfile.txt", contents)
			Expect(stdin.Close()).To(Succeed())
			Expect(session.Wait()).To(Succeed())

			Expect(os.ReadFile(filepath.Join(targetDir, "textfile.txt"))).To(Equal(contents))
			Expect(runner.StartCallCount()).To(BeZero())
			Expect(argsFile).NotTo(BeAnExistingFile())
		})

		It("serves sftp in process and never starts the shell", func() {
			target := filepath.Join(GinkgoT().TempDir(), "textfile.txt")
			sftpClient, err := sftp.NewClient(client)
			Expect(err).NotTo(HaveOccurred())
			defer sftpClient.Close()

			file, err := sftpClient.Create(target)
			Expect(err).NotTo(HaveOccurred())
			_, err = file.Write([]byte("in process"))
			Expect(err).NotTo(HaveOccurred())
			Expect(file.Close()).To(Succeed())

			Expect(os.ReadFile(target)).To(Equal([]byte("in process")))
			Expect(runner.StartCallCount()).To(BeZero())
			Expect(argsFile).NotTo(BeAnExistingFile())
		})
	}

	Context("when the container has no guest agent socket, as for an app that does not run in a VM", func() {
		BeforeEach(func() {
			agent = guestAgentAt(filepath.Join(GinkgoT().TempDir(), "agent.sock"))
		})

		itServesTransfersInProcess()
	})

	Context("when the relay says the guest has no agent", func() {
		BeforeEach(func() {
			relay := startFakeGuestAgentRelay(relayAnswers("off"), nil)
			agent = guestAgentAt(relay.path)
		})

		itServesTransfersInProcess()
	})

	Context("when the relay says the guest's agent is down", func() {
		var relay *fakeGuestAgentRelay

		BeforeEach(func() {
			relay = startFakeGuestAgentRelay(relayAnswers("down"), nil)
			agent = guestAgentAt(relay.path)
		})

		// refused opens a session channel, sends request with payload, and
		// returns what the channel wrote to standard error and its exit
		// status.
		refused := func(request string, payload []byte) (string, *ssh.Request) {
			channel, requests, err := client.OpenChannel("session", nil)
			Expect(err).NotTo(HaveOccurred())
			defer channel.Close()
			accepted, err := channel.SendRequest(request, true, payload)
			Expect(err).NotTo(HaveOccurred())
			Expect(accepted).To(BeTrue())

			stderr, err := io.ReadAll(channel.Stderr())
			Expect(err).NotTo(HaveOccurred())
			var exitStatus *ssh.Request
			Eventually(requests).Should(Receive(&exitStatus))
			return string(stderr), exitStatus
		}

		It("refuses scp with a clear reason and exit status 1, rather than serve the container's files", func() {
			targetDir := GinkgoT().TempDir()
			stderr, exitStatus := refused("exec", ssh.Marshal(struct{ Command string }{"scp -v -t " + targetDir}))
			Expect(stderr).To(Equal("scp is not available right now, because the guest agent of this app's VM is not answering (the guest agent did not answer a ping: the agent is down). Try again shortly.\n"))
			Expect(exitStatus.Type).To(Equal("exit-status"))
			Expect(exitStatus.Payload).To(Equal(ssh.Marshal(struct{ Status uint32 }{1})))
			Expect(os.ReadDir(targetDir)).To(BeEmpty())
			Expect(runner.StartCallCount()).To(BeZero())
			Expect(relay.Lines()).To(Equal([]string{"ping"}))
		})

		It("refuses sftp with a clear reason and exit status 1, rather than serve the container's files", func() {
			stderr, exitStatus := refused("subsystem", ssh.Marshal(struct{ Subsystem string }{"sftp"}))
			Expect(stderr).To(Equal("sftp is not available right now, because the guest agent of this app's VM is not answering (the guest agent did not answer a ping: the agent is down). Try again shortly.\n"))
			Expect(exitStatus.Type).To(Equal("exit-status"))
			Expect(exitStatus.Payload).To(Equal(ssh.Marshal(struct{ Status uint32 }{1})))
			Expect(runner.StartCallCount()).To(BeZero())
			Expect(argsFile).NotTo(BeAnExistingFile())
		})

		It("still runs other commands through the shell", func() {
			output, err := newSession().Output("echo hello")
			Expect(err).NotTo(HaveOccurred())
			Expect(string(output)).To(Equal("hello\n"))
		})
	})

	Context("when there is no guest agent at all, as without -vmContainer", func() {
		itServesTransfersInProcess()
	})
})
