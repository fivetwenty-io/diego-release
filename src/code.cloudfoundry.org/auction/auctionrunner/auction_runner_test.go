package auctionrunner_test

import (
	"errors"
	"os"
	"sync"
	"time"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/lager/v3"
	"code.cloudfoundry.org/workpool"

	"code.cloudfoundry.org/auction/auctionrunner"
	"code.cloudfoundry.org/auction/auctiontypes"
	"code.cloudfoundry.org/auction/auctiontypes/fakes"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/rep"
	"code.cloudfoundry.org/rep/repfakes"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeRunnerDelegate records what the auction runner asks of its delegate and
// resolves execution backends from fixed maps.
type fakeRunnerDelegate struct {
	clients map[string]models.RepClient

	lock          sync.Mutex
	lrpBackends   map[string]string
	taskBackends  map[string]string
	resolveCalls  int
	resolvedLRPs  []auctiontypes.LRPAuction
	resolvedTasks []auctiontypes.TaskAuction

	results chan auctiontypes.AuctionResults
}

func (d *fakeRunnerDelegate) FetchCellReps(lager.Logger, string) (map[string]models.RepClient, error) {
	return d.clients, nil
}

func (d *fakeRunnerDelegate) AuctionCompleted(_ lager.Logger, _ string, results auctiontypes.AuctionResults) {
	d.results <- results
}

func (d *fakeRunnerDelegate) ResolveExecutionBackends(_ lager.Logger, _ string, lrps []auctiontypes.LRPAuction, tasks []auctiontypes.TaskAuction) {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.resolveCalls++
	for i := range lrps {
		if backend, ok := d.lrpBackends[lrps[i].ProcessGuid]; ok {
			lrps[i].ExecutionBackend = backend
		} else {
			lrps[i].PlacementError = "unable to determine execution backend: desired LRP not found"
		}
	}
	for i := range tasks {
		if backend, ok := d.taskBackends[tasks[i].TaskGuid]; ok {
			tasks[i].ExecutionBackend = backend
		} else {
			tasks[i].PlacementError = "unable to determine execution backend: boom"
		}
	}
	d.resolvedLRPs = append(d.resolvedLRPs, lrps...)
	d.resolvedTasks = append(d.resolvedTasks, tasks...)
}

func (d *fakeRunnerDelegate) ResolveCallCount() int {
	d.lock.Lock()
	defer d.lock.Unlock()
	return d.resolveCalls
}

var _ = Describe("AuctionRunner with execution backends", func() {
	var (
		delegate *fakeRunnerDelegate
		workPool *workpool.WorkPool
		signals  chan os.Signal
		done     chan struct{}
	)

	addCellState := func(name string, evacuating bool, optionalPlacementTags []string) *repfakes.FakeSimClient {
		client := &repfakes.FakeSimClient{}
		client.StateReturns(BuildCellState(name, len(delegate.clients), "z1", 100, 100, 100, evacuating, 0, linuxOnlyRootFSProviders, nil, []string{}, []string{}, optionalPlacementTags, 0), nil)
		delegate.clients[name] = client
		return client
	}

	addCell := func(name string, optionalPlacementTags []string) *repfakes.FakeSimClient {
		return addCellState(name, false, optionalPlacementTags)
	}

	vmCellTags := func() []string {
		policy, err := rep.NewExecutionBackendPolicy([]string{"vm-qemu"}, []string{"vm-qemu"}, nil)
		Expect(err).NotTo(HaveOccurred())
		return policy.PlacementTags()
	}

	BeforeEach(func() {
		var err error
		workPool, err = workpool.NewWorkPool(5)
		Expect(err).NotTo(HaveOccurred())

		delegate = &fakeRunnerDelegate{
			clients:      map[string]models.RepClient{},
			lrpBackends:  map[string]string{},
			taskBackends: map[string]string{},
			results:      make(chan auctiontypes.AuctionResults, 1),
		}
		signals = make(chan os.Signal, 1)
		done = make(chan struct{})
	})

	AfterEach(func() {
		signals <- os.Interrupt
		Eventually(done).Should(BeClosed())
		workPool.Stop()
	})

	auction := func(lrps []models.LRPStartRequest, tasks []models.TaskStartRequest) auctiontypes.AuctionResults {
		runner := auctionrunner.New(logger, delegate, &fakes.FakeAuctionMetricEmitterDelegate{}, clock.NewClock(), workPool, 0.0, 0.0, 0)
		ready := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(done)
			Expect(runner.Run(signals, ready)).To(Succeed())
		}()
		Eventually(ready).Should(BeClosed())

		if len(lrps) > 0 {
			runner.ScheduleLRPsForAuctions(lrps, "some-trace-id")
		}
		if len(tasks) > 0 {
			runner.ScheduleTasksForAuctions(tasks, "some-trace-id")
		}

		var results auctiontypes.AuctionResults
		Eventually(delegate.results, 5*time.Second).Should(Receive(&results))
		return results
	}

	Context("when no cell in the zones advertises an execution backend policy", func() {
		var oldClient *repfakes.FakeSimClient

		BeforeEach(func() {
			oldClient = addCell("old-cell", []string{"some-optional-tag"})
		})

		It("still looks up the backends and schedules workloads that select none as before", func() {
			delegate.lrpBackends["pg-1"] = ""

			results := auction(
				[]models.LRPStartRequest{BuildLRPStartRequest("pg-1", "domain", []int{0}, linuxRootFSURL, 10, 10, 10, []string{}, []string{})},
				nil,
			)

			Expect(delegate.ResolveCallCount()).To(Equal(1))
			Expect(results.FailedLRPs).To(BeEmpty())
			Expect(results.SuccessfulLRPs).To(HaveLen(1))
			Expect(results.SuccessfulLRPs[0].Winner).To(Equal("old-cell"))
		})

		Context("because the only cell that offers a VM backend is evacuating", func() {
			BeforeEach(func() {
				addCellState("vm-cell", true, vmCellTags())
			})

			It("does not place a VM workload on a cell that does not offer its backend", func() {
				delegate.lrpBackends["pg-vm"] = "vm-qemu"
				delegate.taskBackends["tg-vm"] = "vm-qemu"

				results := auction(
					[]models.LRPStartRequest{BuildLRPStartRequest("pg-vm", "domain", []int{0}, linuxRootFSURL, 10, 10, 10, []string{}, []string{})},
					[]models.TaskStartRequest{BuildTaskStartRequest("tg-vm", "domain", linuxRootFSURL, 10, 10, 10)},
				)

				Expect(delegate.ResolveCallCount()).To(Equal(1))
				Expect(results.SuccessfulLRPs).To(BeEmpty())
				Expect(results.FailedLRPs).To(HaveLen(1))
				Expect(results.FailedLRPs[0].PlacementError).To(Equal(`found no compatible cell offering execution backend "vm-qemu"`))
				Expect(results.SuccessfulTasks).To(BeEmpty())
				Expect(results.FailedTasks).To(HaveLen(1))
				Expect(results.FailedTasks[0].PlacementError).To(Equal(`found no compatible cell offering execution backend "vm-qemu"`))
				Expect(oldClient.PerformCallCount()).To(Equal(0))
			})
		})

		Context("because the only cell that offers a VM backend did not report its state", func() {
			BeforeEach(func() {
				vmClient := addCell("vm-cell", vmCellTags())
				vmClient.StateReturns(models.CellState{}, errors.New("cell unreachable"))
			})

			It("does not place a VM workload on a cell that does not offer its backend", func() {
				delegate.lrpBackends["pg-vm"] = "vm-qemu"

				results := auction(
					[]models.LRPStartRequest{BuildLRPStartRequest("pg-vm", "domain", []int{0}, linuxRootFSURL, 10, 10, 10, []string{}, []string{})},
					nil,
				)

				Expect(delegate.ResolveCallCount()).To(Equal(1))
				Expect(results.SuccessfulLRPs).To(BeEmpty())
				Expect(results.FailedLRPs).To(HaveLen(1))
				Expect(results.FailedLRPs[0].PlacementError).To(Equal(`found no compatible cell offering execution backend "vm-qemu"`))
				Expect(oldClient.PerformCallCount()).To(Equal(0))
			})
		})
	})

	Context("when a cell advertises an execution backend policy", func() {
		var gvisorClient, oldClient *repfakes.FakeSimClient

		BeforeEach(func() {
			policy, err := rep.NewExecutionBackendPolicy([]string{"oci-gvisor"}, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			gvisorClient = addCell("gvisor-cell", policy.PlacementTags())
			oldClient = addCell("old-cell", []string{})
		})

		It("looks up the backends and places each LRP on a cell that offers its backend", func() {
			delegate.lrpBackends["pg-gvisor"] = "oci-gvisor"

			results := auction(
				[]models.LRPStartRequest{BuildLRPStartRequest("pg-gvisor", "domain", []int{0, 1}, linuxRootFSURL, 10, 10, 10, []string{}, []string{})},
				nil,
			)

			Expect(delegate.ResolveCallCount()).To(Equal(1))
			Expect(results.FailedLRPs).To(BeEmpty())
			Expect(results.SuccessfulLRPs).To(HaveLen(2))
			for _, lrp := range results.SuccessfulLRPs {
				Expect(lrp.Winner).To(Equal("gvisor-cell"))
				Expect(lrp.ExecutionBackend).To(Equal("oci-gvisor"))
			}
			Expect(gvisorClient.PerformCallCount()).To(Equal(1))
			Expect(oldClient.PerformCallCount()).To(Equal(0))
		})

		It("fails the LRP auctions and leaves the task auctions pending whose backend it cannot resolve, without scheduling them", func() {
			delegate.lrpBackends["pg-known"] = ""
			delegate.taskBackends["tg-known"] = "oci-gvisor"

			results := auction(
				[]models.LRPStartRequest{
					BuildLRPStartRequest("pg-known", "domain", []int{0}, linuxRootFSURL, 10, 10, 10, []string{}, []string{}),
					BuildLRPStartRequest("pg-missing", "domain", []int{0}, linuxRootFSURL, 10, 10, 10, []string{}, []string{}),
				},
				[]models.TaskStartRequest{
					BuildTaskStartRequest("tg-known", "domain", linuxRootFSURL, 10, 10, 10),
					BuildTaskStartRequest("tg-missing", "domain", linuxRootFSURL, 10, 10, 10),
				},
			)

			Expect(results.SuccessfulLRPs).To(HaveLen(1))
			Expect(results.SuccessfulLRPs[0].ProcessGuid).To(Equal("pg-known"))

			Expect(results.FailedLRPs).To(HaveLen(1))
			Expect(results.FailedLRPs[0].ProcessGuid).To(Equal("pg-missing"))
			Expect(results.FailedLRPs[0].Attempts).To(Equal(1))
			Expect(results.FailedLRPs[0].PlacementError).To(Equal("unable to determine execution backend: desired LRP not found"))

			Expect(results.SuccessfulTasks).To(HaveLen(1))
			Expect(results.SuccessfulTasks[0].TaskGuid).To(Equal("tg-known"))
			Expect(results.SuccessfulTasks[0].Winner).To(Equal("gvisor-cell"))

			// A failed task auction makes the auctioneer reject the task, which
			// uses up one of its retries, so the round leaves an unresolved
			// task out of the results and BBS offers it again.
			Expect(results.FailedTasks).To(BeEmpty())

			for _, client := range []*repfakes.FakeSimClient{gvisorClient, oldClient} {
				for i := 0; i < client.PerformCallCount(); i++ {
					_, work := client.PerformArgsForCall(i)
					for _, lrp := range work.LRPs {
						Expect(lrp.ProcessGuid).NotTo(Equal("pg-missing"))
					}
					for _, task := range work.Tasks {
						Expect(task.TaskGuid).NotTo(Equal("tg-missing"))
					}
				}
			}
		})
	})
})
