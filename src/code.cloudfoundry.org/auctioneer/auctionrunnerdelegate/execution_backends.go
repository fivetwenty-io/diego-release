package auctionrunnerdelegate

import (
	"errors"
	"sync"

	"code.cloudfoundry.org/auction/auctiontypes"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/lager/v3"
	"code.cloudfoundry.org/rep"
)

const (
	// lrpBackendCacheSize bounds the number of process guids whose backend the
	// delegate remembers. The run info of a desired LRP, which carries the
	// selector, never changes, so a remembered backend stays correct for as long
	// as the process guid names the same desired LRP.
	lrpBackendCacheSize = 10000

	// desiredLRPLookupBatchSize is the most process guids asked of BBS in one
	// request.
	desiredLRPLookupBatchSize = 500

	// taskLookupConcurrency is the most task lookups in flight at once, because
	// BBS looks up tasks only one at a time.
	taskLookupConcurrency = 16
)

const executionBackendLookupFailed = "unable to determine execution backend: "

var errDesiredLRPNotFound = errors.New("desired LRP not found")

// ResolveExecutionBackends looks up the backend that each workload selects,
// because the start requests from BBS do not carry it. It reads the selector
// the same way the rep does, from the desired LRP or the task. A workload whose
// lookup fails gets a placement error, and BBS offers it again later through
// convergence.
func (a *AuctionRunnerDelegate) ResolveExecutionBackends(logger lager.Logger, traceID string, lrps []auctiontypes.LRPAuction, tasks []auctiontypes.TaskAuction) {
	logger = logger.Session("resolve-execution-backends")
	a.resolveLRPBackends(logger, traceID, lrps)
	a.resolveTaskBackends(logger, traceID, tasks)
}

func (a *AuctionRunnerDelegate) resolveLRPBackends(logger lager.Logger, traceID string, lrps []auctiontypes.LRPAuction) {
	if len(lrps) == 0 {
		return
	}

	a.lrpBackendsLock.Lock()
	defer a.lrpBackendsLock.Unlock()

	// Forgetting every backend at once keeps the cache bounded. It happens
	// before the lookup, so every LRP in this auction is either remembered or
	// looked up.
	if len(a.lrpBackends)+len(lrps) > lrpBackendCacheSize {
		a.lrpBackends = map[string]string{}
	}

	missing := []string{}
	seen := map[string]struct{}{}
	for i := range lrps {
		guid := lrps[i].ProcessGuid
		if _, ok := a.lrpBackends[guid]; ok {
			continue
		}
		if _, ok := seen[guid]; ok {
			continue
		}
		seen[guid] = struct{}{}
		missing = append(missing, guid)
	}

	failures := map[string]error{}
	for start := 0; start < len(missing); start += desiredLRPLookupBatchSize {
		end := min(start+desiredLRPLookupBatchSize, len(missing))
		guids := missing[start:end]

		desiredLRPs, err := a.bbsClient.DesiredLRPs(logger, traceID, models.DesiredLRPFilter{ProcessGuids: guids})
		if err != nil {
			logger.Error("failed-to-fetch-desired-lrps", err, lager.Data{"process-guids": len(guids)})
			for _, guid := range guids {
				failures[guid] = err
			}
			continue
		}

		for _, desiredLRP := range desiredLRPs {
			a.lrpBackends[desiredLRP.ProcessGuid] = rep.ExecutionBackendForDesiredLRP(desiredLRP)
		}
		for _, guid := range guids {
			if _, ok := a.lrpBackends[guid]; !ok {
				failures[guid] = errDesiredLRPNotFound
			}
		}
	}

	for i := range lrps {
		lrp := &lrps[i]
		if backend, ok := a.lrpBackends[lrp.ProcessGuid]; ok {
			lrp.ExecutionBackend = backend
			continue
		}
		lrp.PlacementError = executionBackendLookupFailed + failures[lrp.ProcessGuid].Error()
	}
}

func (a *AuctionRunnerDelegate) resolveTaskBackends(logger lager.Logger, traceID string, tasks []auctiontypes.TaskAuction) {
	wg := &sync.WaitGroup{}
	throttle := make(chan struct{}, taskLookupConcurrency)

	for i := range tasks {
		taskAuction := &tasks[i]
		wg.Add(1)
		throttle <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-throttle }()

			task, err := a.bbsClient.TaskByGuid(logger, traceID, taskAuction.TaskGuid)
			if err != nil {
				logger.Error("failed-to-fetch-task", err, lager.Data{"task-guid": taskAuction.TaskGuid})
				taskAuction.PlacementError = executionBackendLookupFailed + err.Error()
				return
			}
			taskAuction.ExecutionBackend = rep.ExecutionBackendForTask(task)
		}()
	}

	wg.Wait()
}
