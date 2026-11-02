package auctionrunner

import (
	"code.cloudfoundry.org/auction/auctiontypes"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("furtherPlacementError", func() {
	var (
		noVM        = auctiontypes.ExecutionBackendMismatchError{Backend: "vm-qemu"}
		needsQEMU   = auctiontypes.ExecutionBackendMismatchError{Backend: "oci-runc", Required: []string{"vm-qemu"}}
		needsFC     = auctiontypes.ExecutionBackendMismatchError{Backend: "oci-runc", Required: []string{"vm-firecracker"}}
		needsBoth   = auctiontypes.ExecutionBackendMismatchError{Backend: "oci-runc", Required: []string{"vm-firecracker", "vm-qemu"}}
		tagMismatch = auctiontypes.NewPlacementTagMismatchError([]string{"vm-seg"})
	)

	// Each entry gives two zone errors and the error that the pair reports,
	// whichever order the zones are visited in. The errors that predate
	// execution backends keep their old precedence, which depends on the
	// order, so the table covers only pairs with a backend mismatch.
	DescribeTable("prefers the error from the furthest check, in either order",
		func(first, second, expected error) {
			Expect(furtherPlacementError(first, second)).To(Equal(expected))
			Expect(furtherPlacementError(second, first)).To(Equal(expected))
		},
		Entry("a backend mismatch over a rootfs mismatch", auctiontypes.ErrorCellMismatch, noVM, noVM),
		Entry("a backend mismatch over a volume driver mismatch", auctiontypes.ErrorVolumeDriverMismatch, needsQEMU, needsQEMU),
		Entry("a backend mismatch over a placement tag mismatch", tagMismatch, needsQEMU, needsQEMU),
		Entry("the backends that two zones require, combined and sorted", needsQEMU, needsFC, needsBoth),
		Entry("the same requirement once when two zones require it", needsQEMU, needsQEMU, needsQEMU),
	)

	It("takes the first error when there is no earlier one", func() {
		Expect(furtherPlacementError(nil, needsQEMU)).To(Equal(needsQEMU))
		Expect(furtherPlacementError(nil, auctiontypes.ErrorCellMismatch)).To(Equal(auctiontypes.ErrorCellMismatch))
	})
})
