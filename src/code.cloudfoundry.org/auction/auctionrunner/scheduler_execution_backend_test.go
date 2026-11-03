package auctionrunner_test

import (
	"time"

	"code.cloudfoundry.org/clock/fakeclock"
	"code.cloudfoundry.org/lager/v3/lagertest"
	"code.cloudfoundry.org/workpool"

	"code.cloudfoundry.org/auction/auctionrunner"
	"code.cloudfoundry.org/auction/auctiontypes"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/rep"
	"code.cloudfoundry.org/rep/repfakes"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Scheduler with execution backends", func() {
	var (
		clients  map[string]*repfakes.FakeSimClient
		zones    map[string]auctionrunner.Zone
		clock    *fakeclock.FakeClock
		workPool *workpool.WorkPool
		logger   *lagertest.TestLogger
		results  auctiontypes.AuctionResults
	)

	policyTags := func(offered, required, exempt []string) []string {
		policy, err := rep.NewExecutionBackendPolicy(offered, required, exempt)
		Expect(err).NotTo(HaveOccurred())
		return policy.PlacementTags()
	}

	addCell := func(zone, name string, rootFSProviders models.RootFSProviders, placementTags, optionalPlacementTags []string) {
		clients[name] = &repfakes.FakeSimClient{}
		zones[zone] = append(zones[zone], auctionrunner.NewCell(
			logger,
			name,
			clients[name],
			BuildCellState(name, len(zones[zone]), zone, 100, 100, 100, false, 0, rootFSProviders, nil, []string{}, placementTags, optionalPlacementTags, 0),
		))
	}

	schedule := func(request auctiontypes.AuctionRequest) auctiontypes.AuctionResults {
		clock.Increment(time.Minute)
		return auctionrunner.NewScheduler(workPool, zones, clock, logger, 0.0, 0.0, 0).Schedule(request)
	}

	lrpAuction := func(processGuid, backend string, placementTags []string) auctiontypes.LRPAuction {
		auction := BuildLRPAuction(processGuid, "domain", 0, linuxRootFSURL, 10, 10, 10, clock.Now(), nil, placementTags)
		auction.ExecutionBackend = backend
		return auction
	}

	taskAuction := func(taskGuid, domain, backend string, placementTags []string) auctiontypes.TaskAuction {
		auction := BuildTaskAuction(BuildTask(taskGuid, domain, linuxRootFSURL, 10, 10, 10, []string{}, placementTags), clock.Now())
		auction.ExecutionBackend = backend
		return auction
	}

	stagingTaskAuction := func(taskGuid, appBackend string, placementTags []string) auctiontypes.TaskAuction {
		auction := taskAuction(taskGuid, "cf-app-staging", "", placementTags)
		auction.AppExecutionBackend = appBackend
		return auction
	}

	BeforeEach(func() {
		clock = fakeclock.NewFakeClock(time.Now())

		var err error
		workPool, err = workpool.NewWorkPool(5)
		Expect(err).NotTo(HaveOccurred())

		logger = lagertest.NewTestLogger("fakelogger")
		clients = map[string]*repfakes.FakeSimClient{}
		zones = map[string]auctionrunner.Zone{}

		// The shared cell has a rep that advertises nothing, like one that
		// predates execution backend policies. The VM cell is in an isolation
		// segment and requires a VM backend for everything except staging. The
		// gvisor cell offers gvisor and still accepts oci-runc.
		addCell("z1", "shared-cell", linuxOnlyRootFSProviders, []string{}, []string{})
		addCell("z1", "vm-cell", linuxOnlyRootFSProviders, []string{"vm-seg"},
			policyTags([]string{"vm-qemu", "vm-firecracker"}, []string{"vm-qemu", "vm-firecracker"}, []string{"cf-app-staging"}))
		addCell("z1", "gvisor-cell", linuxOnlyRootFSProviders, []string{}, policyTags([]string{"oci-gvisor"}, nil, nil))
	})

	AfterEach(func() {
		workPool.Stop()
	})

	Describe("LRPs", func() {
		It("places an LRP that selects no backend on a cell that accepts oci-runc", func() {
			auction := lrpAuction("pg-runc", "", []string{})
			results = schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{auction}})

			Expect(results.FailedLRPs).To(BeEmpty())
			Expect(results.SuccessfulLRPs).To(HaveLen(1))
			Expect(results.SuccessfulLRPs[0].Winner).To(BeElementOf("shared-cell", "gvisor-cell"))
			Expect(clients["vm-cell"].PerformCallCount()).To(Equal(0))
		})

		It("places an LRP that selects gvisor only on the cell that offers it", func() {
			auction := lrpAuction("pg-gvisor", "oci-gvisor", []string{})
			results = schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{auction}})

			Expect(results.FailedLRPs).To(BeEmpty())
			Expect(results.SuccessfulLRPs).To(HaveLen(1))
			Expect(results.SuccessfulLRPs[0].Winner).To(Equal("gvisor-cell"))
			Expect(clients["gvisor-cell"].PerformCallCount()).To(Equal(1))
			Expect(clients["shared-cell"].PerformCallCount()).To(Equal(0))
		})

		It("places an LRP in the segment that selects a VM backend on the VM cell", func() {
			auction := lrpAuction("pg-vm", "vm-qemu", []string{"vm-seg"})
			results = schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{auction}})

			Expect(results.FailedLRPs).To(BeEmpty())
			Expect(results.SuccessfulLRPs).To(HaveLen(1))
			Expect(results.SuccessfulLRPs[0].Winner).To(Equal("vm-cell"))
		})

		It("fails an LRP outside the segment that selects a VM backend and says that no cell offers it", func() {
			auction := lrpAuction("pg-vm", "vm-qemu", []string{})
			results = schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{auction}})

			Expect(results.SuccessfulLRPs).To(BeEmpty())
			Expect(results.FailedLRPs).To(HaveLen(1))
			Expect(results.FailedLRPs[0].Attempts).To(Equal(auction.Attempts + 1))
			Expect(results.FailedLRPs[0].PlacementError).To(Equal(`found no compatible cell offering execution backend "vm-qemu"`))
			for _, client := range clients {
				Expect(client.PerformCallCount()).To(Equal(0))
			}
		})

		It("fails an LRP in the segment that selects no backend and names what the cells require", func() {
			auction := lrpAuction("pg-runc", "", []string{"vm-seg"})
			results = schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{auction}})

			Expect(results.SuccessfulLRPs).To(BeEmpty())
			Expect(results.FailedLRPs).To(HaveLen(1))
			Expect(results.FailedLRPs[0].PlacementError).To(Equal(
				`found no compatible cell accepting execution backend "oci-runc", the cells that offer it require "vm-firecracker" or "vm-qemu"`))
		})

		It("fails an LRP in the segment that selects an offered backend the cells do not require", func() {
			zones = map[string]auctionrunner.Zone{}
			addCell("z1", "mixed-vm-cell", linuxOnlyRootFSProviders, []string{"vm-seg"},
				policyTags([]string{"vm-qemu", "oci-gvisor"}, []string{"vm-qemu"}, nil))

			auction := lrpAuction("pg-gvisor", "oci-gvisor", []string{"vm-seg"})
			results = schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{auction}})

			Expect(results.FailedLRPs).To(HaveLen(1))
			Expect(results.FailedLRPs[0].PlacementError).To(Equal(
				`found no compatible cell accepting execution backend "oci-gvisor", the cells that offer it require "vm-qemu"`))
		})

		It("keeps the rootfs mismatch when no cell matches the rootfs", func() {
			auction := BuildLRPAuction("pg-win", "domain", 0, windowsRootFSURL, 10, 10, 10, clock.Now(), nil, []string{})
			auction.ExecutionBackend = "vm-qemu"
			results = schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{auction}})

			Expect(results.FailedLRPs).To(HaveLen(1))
			Expect(results.FailedLRPs[0].PlacementError).To(Equal(auctiontypes.ErrorCellMismatch.Error()))
		})

		It("keeps the placement tag mismatch when no cell matches the tags", func() {
			auction := lrpAuction("pg-vm", "vm-qemu", []string{"other-seg"})
			results = schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{auction}})

			Expect(results.FailedLRPs).To(HaveLen(1))
			Expect(results.FailedLRPs[0].PlacementError).To(Equal(auctiontypes.NewPlacementTagMismatchError([]string{"other-seg"}).Error()))
		})

		It("reports the backend mismatch over a rootfs mismatch in another zone", func() {
			addCell("z2", "windows-cell", windowsOnlyRootFSProviders, []string{}, []string{})

			auction := lrpAuction("pg-vm", "vm-firecracker", []string{})
			results = schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{auction}})

			Expect(results.FailedLRPs).To(HaveLen(1))
			Expect(results.FailedLRPs[0].PlacementError).To(Equal(`found no compatible cell offering execution backend "vm-firecracker"`))
		})

		It("combines what the cells require across zones", func() {
			addCell("z2", "qemu-only-cell", linuxOnlyRootFSProviders, []string{"vm-seg"},
				policyTags([]string{"vm-qemu"}, []string{"vm-qemu"}, nil))
			zones["z1"] = auctionrunner.Zone{}
			addCell("z1", "firecracker-only-cell", linuxOnlyRootFSProviders, []string{"vm-seg"},
				policyTags([]string{"vm-firecracker"}, []string{"vm-firecracker"}, nil))

			auction := lrpAuction("pg-runc", "", []string{"vm-seg"})
			results = schedule(auctiontypes.AuctionRequest{LRPs: []auctiontypes.LRPAuction{auction}})

			Expect(results.FailedLRPs).To(HaveLen(1))
			Expect(results.FailedLRPs[0].PlacementError).To(Equal(
				`found no compatible cell accepting execution backend "oci-runc", the cells that offer it require "vm-firecracker" or "vm-qemu"`))
		})
	})

	Describe("tasks", func() {
		It("places a staging task in the segment on the VM cell when the app it builds selects a backend the cell requires", func() {
			auction := stagingTaskAuction("tg-stage", "vm-firecracker", []string{"vm-seg"})
			results = schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{auction}})

			Expect(results.FailedTasks).To(BeEmpty())
			Expect(results.SuccessfulTasks).To(HaveLen(1))
			Expect(results.SuccessfulTasks[0].Winner).To(Equal("vm-cell"))
		})

		It("fails a staging task in the segment when the app it builds selects no backend", func() {
			auction := stagingTaskAuction("tg-stage", "", []string{"vm-seg"})
			results = schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{auction}})

			Expect(results.SuccessfulTasks).To(BeEmpty())
			Expect(results.FailedTasks).To(HaveLen(1))
			Expect(results.FailedTasks[0].PlacementError).To(Equal(
				`found no compatible cell accepting execution backend "oci-runc", the cells that offer it require "vm-firecracker" or "vm-qemu"`))
			Expect(clients["vm-cell"].PerformCallCount()).To(Equal(0))
		})

		It("fails a staging task in the segment when the app it builds selects a backend the cell does not require", func() {
			auction := stagingTaskAuction("tg-stage", "oci-gvisor", []string{"vm-seg"})
			results = schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{auction}})

			Expect(results.SuccessfulTasks).To(BeEmpty())
			Expect(results.FailedTasks).To(HaveLen(1))
		})

		It("places a staging task outside the segment on a cell that accepts oci-runc, whatever the app selects", func() {
			auction := stagingTaskAuction("tg-stage", "vm-qemu", []string{})
			results = schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{auction}})

			Expect(results.FailedTasks).To(BeEmpty())
			Expect(results.SuccessfulTasks).To(HaveLen(1))
			Expect(results.SuccessfulTasks[0].Winner).To(BeElementOf("shared-cell", "gvisor-cell"))
		})

		It("fails another task in the segment that selects no backend and names what the cells require", func() {
			auction := taskAuction("tg-run", "cf-tasks", "", []string{"vm-seg"})
			results = schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{auction}})

			Expect(results.SuccessfulTasks).To(BeEmpty())
			Expect(results.FailedTasks).To(HaveLen(1))
			Expect(results.FailedTasks[0].Attempts).To(Equal(auction.Attempts + 1))
			Expect(results.FailedTasks[0].PlacementError).To(Equal(
				`found no compatible cell accepting execution backend "oci-runc", the cells that offer it require "vm-firecracker" or "vm-qemu"`))
		})

		It("places a task in the segment that selects a VM backend on the VM cell", func() {
			auction := taskAuction("tg-vm", "cf-tasks", "vm-firecracker", []string{"vm-seg"})
			results = schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{auction}})

			Expect(results.FailedTasks).To(BeEmpty())
			Expect(results.SuccessfulTasks).To(HaveLen(1))
			Expect(results.SuccessfulTasks[0].Winner).To(Equal("vm-cell"))
		})

		It("fails a task outside the segment that selects a VM backend and says that no cell offers it", func() {
			auction := taskAuction("tg-vm", "cf-tasks", "vm-firecracker", []string{})
			results = schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{auction}})

			Expect(results.SuccessfulTasks).To(BeEmpty())
			Expect(results.FailedTasks).To(HaveLen(1))
			Expect(results.FailedTasks[0].PlacementError).To(Equal(`found no compatible cell offering execution backend "vm-firecracker"`))
		})

		It("places a task that selects gvisor only on the cell that offers it", func() {
			auction := taskAuction("tg-gvisor", "cf-tasks", "oci-gvisor", []string{})
			results = schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{auction}})

			Expect(results.SuccessfulTasks).To(HaveLen(1))
			Expect(results.SuccessfulTasks[0].Winner).To(Equal("gvisor-cell"))
		})

		It("places a task on the zone whose cell offers its backend when another zone's cell does not", func() {
			addCell("z2", "qemu-cell", linuxOnlyRootFSProviders, []string{"vm-seg-2"},
				policyTags([]string{"vm-qemu"}, []string{"vm-qemu"}, nil))
			addCell("z1", "plain-seg-2-cell", linuxOnlyRootFSProviders, []string{"vm-seg-2"}, []string{})

			auction := taskAuction("tg-vm", "cf-tasks", "vm-qemu", []string{"vm-seg-2"})
			results = schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{auction}})

			Expect(results.FailedTasks).To(BeEmpty())
			Expect(results.SuccessfulTasks).To(HaveLen(1))
			Expect(results.SuccessfulTasks[0].Winner).To(Equal("qemu-cell"))
			Expect(clients["plain-seg-2-cell"].PerformCallCount()).To(Equal(0))
		})

		It("combines what the cells require across zones for a task", func() {
			addCell("z2", "qemu-only-cell", linuxOnlyRootFSProviders, []string{"vm-seg-2"},
				policyTags([]string{"vm-qemu"}, []string{"vm-qemu"}, nil))
			addCell("z3", "windows-cell", windowsOnlyRootFSProviders, []string{}, []string{})
			addCell("z1", "firecracker-only-cell", linuxOnlyRootFSProviders, []string{"vm-seg-2"},
				policyTags([]string{"vm-firecracker"}, []string{"vm-firecracker"}, nil))

			auction := taskAuction("tg-runc", "cf-tasks", "", []string{"vm-seg-2"})
			results = schedule(auctiontypes.AuctionRequest{Tasks: []auctiontypes.TaskAuction{auction}})

			Expect(results.SuccessfulTasks).To(BeEmpty())
			Expect(results.FailedTasks).To(HaveLen(1))
			Expect(results.FailedTasks[0].PlacementError).To(Equal(
				`found no compatible cell accepting execution backend "oci-runc", the cells that offer it require "vm-firecracker" or "vm-qemu"`))
		})
	})
})
