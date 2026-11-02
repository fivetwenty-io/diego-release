package auctionrunnerdelegate_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"code.cloudfoundry.org/auction/auctiontypes"
	"code.cloudfoundry.org/bbs/fake_bbs"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/bbs/models/test/model_helpers"
	"code.cloudfoundry.org/clock/fakeclock"

	"code.cloudfoundry.org/rep/repfakes"

	"code.cloudfoundry.org/lager/v3"
	"code.cloudfoundry.org/lager/v3/lagertest"

	"code.cloudfoundry.org/auctioneer/auctionrunnerdelegate"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Resolving execution backends", func() {
	var (
		delegate    *auctionrunnerdelegate.AuctionRunnerDelegate
		bbsClient   *fake_bbs.FakeInternalClient
		fakeClock   *fakeclock.FakeClock
		logger      lager.Logger
		desiredLRPs map[string]*models.DesiredLRP
	)

	const lookupTimeout = 3 * time.Second

	desiredLRPWithBackend := func(processGuid, backend string) *models.DesiredLRP {
		desiredLRP := model_helpers.NewValidDesiredLRP(processGuid)
		if backend != "" {
			desiredLRP.Action = models.WrapAction(&models.RunAction{
				Path: "/tmp/lifecycle/launcher",
				User: "vcap",
				Env:  []*models.EnvironmentVariable{{Name: "CF_EXECUTION_BACKEND", Value: backend}},
			})
		}
		return desiredLRP
	}

	lrpAuction := func(processGuid string, index int32) auctiontypes.LRPAuction {
		return auctiontypes.NewLRPAuction(
			models.NewSchedulingLRP("", models.NewActualLRPKey(processGuid, index, "domain"), models.NewResource(10, 10, 10), models.NewPlacementConstraint("preloaded:linux", nil, nil)),
			time.Now(),
		)
	}

	taskAuction := func(taskGuid string) auctiontypes.TaskAuction {
		return auctiontypes.NewTaskAuction(
			models.NewSchedulingTask(taskGuid, "domain", models.NewResource(10, 10, 10), models.NewPlacementConstraint("preloaded:linux", nil, nil)),
			time.Now(),
		)
	}

	BeforeEach(func() {
		bbsClient = &fake_bbs.FakeInternalClient{}
		logger = lagertest.NewTestLogger("delegate")
		fakeClock = fakeclock.NewFakeClock(time.Now())
		delegate = auctionrunnerdelegate.New(&repfakes.FakeClientFactory{}, bbsClient, fakeClock, lookupTimeout)

		desiredLRPs = map[string]*models.DesiredLRP{}
		bbsClient.DesiredLRPsStub = func(_ lager.Logger, _ string, filter models.DesiredLRPFilter) ([]*models.DesiredLRP, error) {
			found := []*models.DesiredLRP{}
			for _, guid := range filter.ProcessGuids {
				if desiredLRP, ok := desiredLRPs[guid]; ok {
					found = append(found, desiredLRP)
				}
			}
			return found, nil
		}
	})

	Describe("LRPs", func() {
		It("does not call BBS when there is nothing to resolve", func() {
			delegate.ResolveExecutionBackends(logger, "trace-id", nil, nil)
			Expect(bbsClient.DesiredLRPsCallCount()).To(Equal(0))
			Expect(bbsClient.TaskByGuidCallCount()).To(Equal(0))
		})

		It("reads each LRP's backend from its desired LRP and asks for each process guid once", func() {
			desiredLRPs["pg-vm"] = desiredLRPWithBackend("pg-vm", "vm-qemu")
			desiredLRPs["pg-runc"] = desiredLRPWithBackend("pg-runc", "")

			lrps := []auctiontypes.LRPAuction{lrpAuction("pg-vm", 0), lrpAuction("pg-vm", 1), lrpAuction("pg-runc", 0)}
			delegate.ResolveExecutionBackends(logger, "trace-id", lrps, nil)

			Expect(lrps[0].ExecutionBackend).To(Equal("vm-qemu"))
			Expect(lrps[1].ExecutionBackend).To(Equal("vm-qemu"))
			Expect(lrps[2].ExecutionBackend).To(Equal(""))
			for _, lrp := range lrps {
				Expect(lrp.PlacementError).To(BeEmpty())
			}

			Expect(bbsClient.DesiredLRPsCallCount()).To(Equal(1))
			_, traceID, filter := bbsClient.DesiredLRPsArgsForCall(0)
			Expect(traceID).To(Equal("trace-id"))
			Expect(filter.ProcessGuids).To(ConsistOf("pg-vm", "pg-runc"))
		})

		It("remembers a resolved backend and does not ask BBS for it again", func() {
			desiredLRPs["pg-vm"] = desiredLRPWithBackend("pg-vm", "vm-firecracker")

			delegate.ResolveExecutionBackends(logger, "trace-id", []auctiontypes.LRPAuction{lrpAuction("pg-vm", 0)}, nil)
			Expect(bbsClient.DesiredLRPsCallCount()).To(Equal(1))

			lrps := []auctiontypes.LRPAuction{lrpAuction("pg-vm", 3)}
			delegate.ResolveExecutionBackends(logger, "trace-id", lrps, nil)
			Expect(bbsClient.DesiredLRPsCallCount()).To(Equal(1))
			Expect(lrps[0].ExecutionBackend).To(Equal("vm-firecracker"))
		})

		It("gives an LRP whose desired LRP is missing a placement error and asks again next time", func() {
			lrps := []auctiontypes.LRPAuction{lrpAuction("pg-gone", 0)}
			delegate.ResolveExecutionBackends(logger, "trace-id", lrps, nil)
			Expect(lrps[0].PlacementError).To(Equal("unable to determine execution backend: desired LRP not found"))
			Expect(lrps[0].ExecutionBackend).To(BeEmpty())

			desiredLRPs["pg-gone"] = desiredLRPWithBackend("pg-gone", "oci-gvisor")
			lrps = []auctiontypes.LRPAuction{lrpAuction("pg-gone", 0)}
			delegate.ResolveExecutionBackends(logger, "trace-id", lrps, nil)
			Expect(bbsClient.DesiredLRPsCallCount()).To(Equal(2))
			Expect(lrps[0].PlacementError).To(BeEmpty())
			Expect(lrps[0].ExecutionBackend).To(Equal("oci-gvisor"))
		})

		It("gives every LRP in a failed lookup a placement error and keeps the others", func() {
			desiredLRPs["pg-cached"] = desiredLRPWithBackend("pg-cached", "vm-qemu")
			delegate.ResolveExecutionBackends(logger, "trace-id", []auctiontypes.LRPAuction{lrpAuction("pg-cached", 0)}, nil)

			bbsClient.DesiredLRPsStub = nil
			bbsClient.DesiredLRPsReturns(nil, errors.New("bbs is down"))

			lrps := []auctiontypes.LRPAuction{lrpAuction("pg-cached", 1), lrpAuction("pg-new", 0)}
			delegate.ResolveExecutionBackends(logger, "trace-id", lrps, nil)

			Expect(lrps[0].PlacementError).To(BeEmpty())
			Expect(lrps[0].ExecutionBackend).To(Equal("vm-qemu"))
			Expect(lrps[1].PlacementError).To(Equal("unable to determine execution backend: bbs is down"))
		})

		It("asks BBS in batches", func() {
			lrps := []auctiontypes.LRPAuction{}
			for i := 0; i < 1201; i++ {
				guid := fmt.Sprintf("pg-%d", i)
				desiredLRPs[guid] = desiredLRPWithBackend(guid, "oci-gvisor")
				lrps = append(lrps, lrpAuction(guid, 0))
			}

			delegate.ResolveExecutionBackends(logger, "trace-id", lrps, nil)

			Expect(bbsClient.DesiredLRPsCallCount()).To(Equal(3))
			total := 0
			for i := 0; i < 3; i++ {
				_, _, filter := bbsClient.DesiredLRPsArgsForCall(i)
				Expect(len(filter.ProcessGuids)).To(BeNumerically("<=", 500))
				total += len(filter.ProcessGuids)
			}
			Expect(total).To(Equal(1201))
			for _, lrp := range lrps {
				Expect(lrp.ExecutionBackend).To(Equal("oci-gvisor"))
			}
		})

		It("still resolves a remembered LRP in the auction that fills the cache", func() {
			desiredLRPs["pg-remembered"] = desiredLRPWithBackend("pg-remembered", "vm-firecracker")
			delegate.ResolveExecutionBackends(logger, "trace-id", []auctiontypes.LRPAuction{lrpAuction("pg-remembered", 0)}, nil)

			lrps := []auctiontypes.LRPAuction{lrpAuction("pg-remembered", 1)}
			for i := 0; i < 10000; i++ {
				guid := fmt.Sprintf("pg-%d", i)
				desiredLRPs[guid] = desiredLRPWithBackend(guid, "vm-qemu")
				lrps = append(lrps, lrpAuction(guid, 0))
			}
			delegate.ResolveExecutionBackends(logger, "trace-id", lrps, nil)

			Expect(lrps[0].PlacementError).To(BeEmpty())
			Expect(lrps[0].ExecutionBackend).To(Equal("vm-firecracker"))
			for _, lrp := range lrps[1:] {
				Expect(lrp.PlacementError).To(BeEmpty())
				Expect(lrp.ExecutionBackend).To(Equal("vm-qemu"))
			}
		})

		It("looks a remembered backend up again once it is ten minutes old", func() {
			desiredLRPs["pg-redesired"] = desiredLRPWithBackend("pg-redesired", "vm-qemu")
			delegate.ResolveExecutionBackends(logger, "trace-id", []auctiontypes.LRPAuction{lrpAuction("pg-redesired", 0)}, nil)

			desiredLRPs["pg-redesired"] = desiredLRPWithBackend("pg-redesired", "oci-gvisor")
			fakeClock.Increment(9 * time.Minute)
			lrps := []auctiontypes.LRPAuction{lrpAuction("pg-redesired", 0)}
			delegate.ResolveExecutionBackends(logger, "trace-id", lrps, nil)
			Expect(bbsClient.DesiredLRPsCallCount()).To(Equal(1))
			Expect(lrps[0].ExecutionBackend).To(Equal("vm-qemu"))

			fakeClock.Increment(time.Minute)
			lrps = []auctiontypes.LRPAuction{lrpAuction("pg-redesired", 0)}
			delegate.ResolveExecutionBackends(logger, "trace-id", lrps, nil)
			Expect(bbsClient.DesiredLRPsCallCount()).To(Equal(2))
			Expect(lrps[0].ExecutionBackend).To(Equal("oci-gvisor"))
		})

		It("asks BBS for several batches at once, but no more than four", func() {
			var inFlight, maxInFlight int32
			release := make(chan struct{})
			bbsClient.DesiredLRPsStub = func(_ lager.Logger, _ string, filter models.DesiredLRPFilter) ([]*models.DesiredLRP, error) {
				current := atomic.AddInt32(&inFlight, 1)
				defer atomic.AddInt32(&inFlight, -1)
				for {
					seen := atomic.LoadInt32(&maxInFlight)
					if current <= seen || atomic.CompareAndSwapInt32(&maxInFlight, seen, current) {
						break
					}
				}
				<-release
				found := []*models.DesiredLRP{}
				for _, guid := range filter.ProcessGuids {
					found = append(found, desiredLRPWithBackend(guid, "vm-qemu"))
				}
				return found, nil
			}

			lrps := []auctiontypes.LRPAuction{}
			for i := 0; i < 3000; i++ {
				lrps = append(lrps, lrpAuction(fmt.Sprintf("pg-%d", i), 0))
			}

			done := make(chan struct{})
			go func() {
				defer close(done)
				delegate.ResolveExecutionBackends(logger, "trace-id", lrps, nil)
			}()

			Eventually(func() int32 { return atomic.LoadInt32(&inFlight) }).Should(Equal(int32(4)))
			Consistently(func() int32 { return atomic.LoadInt32(&inFlight) }, 100*time.Millisecond).Should(Equal(int32(4)))
			close(release)
			Eventually(done).Should(BeClosed())

			Expect(atomic.LoadInt32(&maxInFlight)).To(Equal(int32(4)))
			Expect(bbsClient.DesiredLRPsCallCount()).To(Equal(6))
			for _, lrp := range lrps {
				Expect(lrp.PlacementError).To(BeEmpty())
				Expect(lrp.ExecutionBackend).To(Equal("vm-qemu"))
			}
		})

		It("keeps resolving correctly after the cache fills up", func() {
			for round := 0; round < 3; round++ {
				lrps := []auctiontypes.LRPAuction{}
				for i := 0; i < 4000; i++ {
					guid := fmt.Sprintf("pg-%d-%d", round, i)
					desiredLRPs[guid] = desiredLRPWithBackend(guid, "vm-qemu")
					lrps = append(lrps, lrpAuction(guid, 0))
				}
				delegate.ResolveExecutionBackends(logger, "trace-id", lrps, nil)
				for _, lrp := range lrps {
					Expect(lrp.PlacementError).To(BeEmpty())
					Expect(lrp.ExecutionBackend).To(Equal("vm-qemu"))
				}
			}
		})
	})

	Describe("tasks", func() {
		It("reads each task's backend from the task", func() {
			vmTask := model_helpers.NewValidTask("tg-vm")
			vmTask.EnvironmentVariables = append(vmTask.EnvironmentVariables, &models.EnvironmentVariable{Name: "CF_EXECUTION_BACKEND", Value: "vm-qemu"})
			plainTask := model_helpers.NewValidTask("tg-plain")

			bbsClient.TaskByGuidStub = func(_ lager.Logger, _ string, guid string) (*models.Task, error) {
				switch guid {
				case "tg-vm":
					return vmTask, nil
				case "tg-plain":
					return plainTask, nil
				}
				return nil, models.ErrResourceNotFound
			}

			tasks := []auctiontypes.TaskAuction{taskAuction("tg-vm"), taskAuction("tg-plain"), taskAuction("tg-gone")}
			delegate.ResolveExecutionBackends(logger, "trace-id", nil, tasks)

			Expect(bbsClient.TaskByGuidCallCount()).To(Equal(3))
			Expect(tasks[0].ExecutionBackend).To(Equal("vm-qemu"))
			Expect(tasks[0].PlacementError).To(BeEmpty())
			Expect(tasks[1].ExecutionBackend).To(Equal(""))
			Expect(tasks[1].PlacementError).To(BeEmpty())
			Expect(tasks[2].PlacementError).To(Equal("unable to determine execution backend: " + models.ErrResourceNotFound.Error()))
		})

		It("reads the backend that the app a staging task builds selects from the task's run action", func() {
			stagingTask := model_helpers.NewValidTask("tg-staging")
			stagingTask.Domain = "cf-app-staging"
			stagingTask.Action = models.WrapAction(&models.RunAction{
				Path: "/tmp/lifecycle/builder",
				User: "vcap",
				Env:  []*models.EnvironmentVariable{{Name: "CF_EXECUTION_BACKEND", Value: "vm-firecracker"}},
			})
			bbsClient.TaskByGuidReturns(stagingTask, nil)

			tasks := []auctiontypes.TaskAuction{taskAuction("tg-staging")}
			delegate.ResolveExecutionBackends(logger, "trace-id", nil, tasks)

			Expect(tasks[0].PlacementError).To(BeEmpty())
			Expect(tasks[0].ExecutionBackend).To(Equal(""))
			Expect(tasks[0].AppExecutionBackend).To(Equal("vm-firecracker"))
		})
	})

	Describe("the lookup deadline", func() {
		// Lookups that a round gave up on finish after the spec that started
		// them, so each spec gets its own gate, and the stubs hold on to it.
		type lookupGate struct {
			release          chan struct{}
			releaseOnce      sync.Once
			tasksInFlight    int32
			maxTasksInFlight int32
		}

		var gate *lookupGate

		openGate := func() {
			gate.releaseOnce.Do(func() { close(gate.release) })
		}

		tasksInFlight := func() int32 { return atomic.LoadInt32(&gate.tasksInFlight) }

		resolveInBackground := func(lrps []auctiontypes.LRPAuction, tasks []auctiontypes.TaskAuction) chan struct{} {
			done := make(chan struct{})
			go func() {
				defer close(done)
				delegate.ResolveExecutionBackends(logger, "trace-id", lrps, tasks)
			}()
			return done
		}

		BeforeEach(func() {
			g := &lookupGate{release: make(chan struct{})}
			gate = g

			bbsClient.TaskByGuidStub = func(_ lager.Logger, _ string, guid string) (*models.Task, error) {
				current := atomic.AddInt32(&g.tasksInFlight, 1)
				defer atomic.AddInt32(&g.tasksInFlight, -1)
				for {
					seen := atomic.LoadInt32(&g.maxTasksInFlight)
					if current <= seen || atomic.CompareAndSwapInt32(&g.maxTasksInFlight, seen, current) {
						break
					}
				}
				<-g.release
				return model_helpers.NewValidTask(guid), nil
			}
			bbsClient.DesiredLRPsStub = func(_ lager.Logger, _ string, filter models.DesiredLRPFilter) ([]*models.DesiredLRP, error) {
				<-g.release
				found := []*models.DesiredLRP{}
				for _, guid := range filter.ProcessGuids {
					found = append(found, desiredLRPWithBackend(guid, "vm-qemu"))
				}
				return found, nil
			}
		})

		AfterEach(func() {
			openGate()
		})

		It("gives up on a slow BBS at the deadline and leaves every workload it did not hear about unresolved", func() {
			desiredLRPs["pg-remembered"] = desiredLRPWithBackend("pg-remembered", "oci-gvisor")
			stub := bbsClient.DesiredLRPsStub
			bbsClient.DesiredLRPsStub = nil
			bbsClient.DesiredLRPsReturns([]*models.DesiredLRP{desiredLRPs["pg-remembered"]}, nil)
			delegate.ResolveExecutionBackends(logger, "trace-id", []auctiontypes.LRPAuction{lrpAuction("pg-remembered", 0)}, nil)
			bbsClient.DesiredLRPsStub = stub

			lrps := []auctiontypes.LRPAuction{lrpAuction("pg-remembered", 1), lrpAuction("pg-slow", 0)}
			tasks := []auctiontypes.TaskAuction{taskAuction("tg-slow")}
			done := resolveInBackground(lrps, tasks)

			Eventually(bbsClient.DesiredLRPsCallCount).Should(Equal(2))
			Eventually(bbsClient.TaskByGuidCallCount).Should(Equal(1))
			Consistently(done).ShouldNot(BeClosed())

			fakeClock.WaitForWatcherAndIncrement(lookupTimeout)
			Eventually(done).Should(BeClosed())

			Expect(lrps[0].PlacementError).To(BeEmpty())
			Expect(lrps[0].ExecutionBackend).To(Equal("oci-gvisor"))
			Expect(lrps[1].PlacementError).To(Equal("unable to determine execution backend: timed out looking it up"))
			Expect(tasks[0].PlacementError).To(Equal("unable to determine execution backend: timed out looking it up"))
		})

		It("remembers an LRP backend that arrives after the deadline for the next round", func() {
			lrps := []auctiontypes.LRPAuction{lrpAuction("pg-late", 0)}
			done := resolveInBackground(lrps, nil)
			Eventually(bbsClient.DesiredLRPsCallCount).Should(Equal(1))
			fakeClock.WaitForWatcherAndIncrement(lookupTimeout)
			Eventually(done).Should(BeClosed())
			Expect(lrps[0].PlacementError).NotTo(BeEmpty())

			// The lookup that is still in flight keeps the stub it started with,
			// so from here on only that late answer can teach the delegate the
			// backend.
			bbsClient.DesiredLRPsReturns(nil, errors.New("bbs is down"))
			openGate()
			Eventually(func() string {
				next := []auctiontypes.LRPAuction{lrpAuction("pg-late", 1)}
				delegate.ResolveExecutionBackends(logger, "trace-id", next, nil)
				return next[0].ExecutionBackend
			}).Should(Equal("vm-qemu"))
		})

		It("keeps sixteen or fewer task lookups in flight, even across rounds that time out", func() {
			first := []auctiontypes.TaskAuction{}
			for i := 0; i < 40; i++ {
				first = append(first, taskAuction(fmt.Sprintf("tg-first-%d", i)))
			}
			done := resolveInBackground(nil, first)
			Eventually(tasksInFlight).Should(Equal(int32(16)))
			fakeClock.WaitForWatcherAndIncrement(lookupTimeout)
			Eventually(done).Should(BeClosed())
			for _, task := range first {
				Expect(task.PlacementError).To(Equal("unable to determine execution backend: timed out looking it up"))
			}

			second := []auctiontypes.TaskAuction{taskAuction("tg-second")}
			done = resolveInBackground(nil, second)
			Consistently(tasksInFlight, 100*time.Millisecond).Should(Equal(int32(16)))
			fakeClock.WaitForWatcherAndIncrement(lookupTimeout)
			Eventually(done).Should(BeClosed())
			Expect(second[0].PlacementError).To(Equal("unable to determine execution backend: timed out looking it up"))

			openGate()
			Eventually(tasksInFlight).Should(Equal(int32(0)))
			Expect(atomic.LoadInt32(&gate.maxTasksInFlight)).To(Equal(int32(16)))
			Expect(bbsClient.TaskByGuidCallCount()).To(Equal(16))
		})

		It("finishes without waiting for the deadline when BBS answers in time", func() {
			openGate()
			tasks := []auctiontypes.TaskAuction{taskAuction("tg-quick")}
			lrps := []auctiontypes.LRPAuction{lrpAuction("pg-quick", 0)}
			delegate.ResolveExecutionBackends(logger, "trace-id", lrps, tasks)
			Expect(tasks[0].PlacementError).To(BeEmpty())
			Expect(lrps[0].ExecutionBackend).To(Equal("vm-qemu"))
		})
	})
})
