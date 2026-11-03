//go:build !windows2012R2

package main_test

import (
	"fmt"

	"code.cloudfoundry.org/diego-ssh/cmd/sshd/testrunner"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"github.com/tedsuo/ifrit"
)

// guestAgentDir is where the sshd that the specs build looks for the guest
// agent's socket and for its mount table, relative to its working
// directory. The specs' sshd thus never sees a socket at the standard path
// on the host, and a spec opens the gate only by starting sshd in a
// directory that holds both (see guest_agent_test.go).
const guestAgentDir = "guest-agent"

func buildSshd() string {
	ldflags := fmt.Sprintf("-X main.guestAgentSocketPath=%s/vm-launch-agent.sock -X main.guestAgentMountInfoPath=%s/mountinfo", guestAgentDir, guestAgentDir)
	sshd, err := gexec.Build("code.cloudfoundry.org/diego-ssh/cmd/sshd", "-race", "-ldflags", ldflags)
	Expect(err).NotTo(HaveOccurred())
	return sshd
}

func startSshd(sshdPath string, args testrunner.Args, address string, port int) (ifrit.Runner, ifrit.Process) {
	args.Address = fmt.Sprintf("%s:%d", address, port)
	runner := testrunner.New(sshdPath, args)
	process := ifrit.Invoke(runner)
	return runner, process
}
