package testrunner

import (
	"os/exec"
	"strconv"
	"time"

	ginkgomon "github.com/tedsuo/ifrit/ginkgomon_v2"
)

type Args struct {
	Address                     string
	HostKey                     string
	AuthorizedKey               string
	AllowedCiphers              string
	AllowedMACs                 string
	AllowedKeyExchanges         string
	AllowedHostKeyAlgorithms    string
	AllowUnauthenticatedClients bool
	InheritDaemonEnv            bool
	VMContainer                 bool
}

func (args Args) ArgSlice() []string {
	argSlice := []string{
		"-address=" + args.Address,
		"-hostKey=" + args.HostKey,
		"-authorizedKey=" + args.AuthorizedKey,
		"-allowedCiphers=" + args.AllowedCiphers,
		"-allowedMACs=" + args.AllowedMACs,
		"-allowedKeyExchanges=" + args.AllowedKeyExchanges,
		"-allowedHostKeyAlgorithms=" + args.AllowedHostKeyAlgorithms,
		"-allowUnauthenticatedClients=" + strconv.FormatBool(args.AllowUnauthenticatedClients),
		"-inheritDaemonEnv=" + strconv.FormatBool(args.InheritDaemonEnv),
	}
	// The executor appends the flag bare, and only in a VM container.
	if args.VMContainer {
		argSlice = append(argSlice, "-vmContainer")
	}
	return argSlice
}

func New(binPath string, args Args) *ginkgomon.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "sshd",
		AnsiColorCode:     "1;96m",
		StartCheck:        "sshd.started",
		StartCheckTimeout: 10 * time.Second,
		Command:           exec.Command(binPath, args.ArgSlice()...),
	})
}
