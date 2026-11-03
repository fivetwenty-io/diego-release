package auctionrunnerdelegate

import (
	"errors"
	"sync"
	"time"

	"code.cloudfoundry.org/auction/auctiontypes"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/lager/v3"
	"code.cloudfoundry.org/rep"
)

const (
	// DefaultExecutionBackendLookupTimeout is how long one auction round waits
	// for BBS to say which backend its workloads select, when the
	// configuration does not set a timeout.
	DefaultExecutionBackendLookupTimeout = 5 * time.Second

	// lrpBackendCacheSize bounds the number of process guids whose backend the
	// delegate remembers.
	lrpBackendCacheSize = 10000

	// lrpBackendCacheTTL is how long the delegate trusts a remembered backend.
	// The run info of a desired LRP, which carries the selector, never changes,
	// but a process guid can be deleted and desired again with another
	// selector, so a remembered backend is looked up again after this long.
	lrpBackendCacheTTL = 10 * time.Minute

	// desiredLRPLookupBatchSize is the most process guids asked of BBS in one
	// request.
	desiredLRPLookupBatchSize = 500

	// lrpLookupConcurrency is the most desired LRP requests in flight at once.
	lrpLookupConcurrency = 4

	// taskLookupConcurrency is the most task lookups in flight at once,
	// because BBS looks up tasks only one at a time.
	taskLookupConcurrency = 16
)

const executionBackendLookupFailed = "unable to determine execution backend: "

var (
	errDesiredLRPNotFound             = errors.New("desired LRP not found")
	errExecutionBackendLookupTimedOut = errors.New("timed out looking it up")
)

type rememberedBackend struct {
	backend   string
	expiresAt time.Time
}

// ResolveExecutionBackends looks up the backend that each workload selects,
// because the start requests from BBS do not carry it. It reads the selector
// the same way the rep does, from the desired LRP or the task. The whole
// lookup has one deadline, because the auction loop waits for it. A workload
// whose lookup fails or misses the deadline gets a placement error, and BBS
// offers it again later through convergence.
//
// When no cell presence in the latest fetch advertises a policy, every cell
// offers only oci-runc and requires nothing, so the delegate skips the lookup
// and a foundation that never sets a policy pays nothing for it.
//
// The lookups that are in flight count against the concurrency limits until
// BBS answers them, even after their round has given up on them, so a slow
// BBS never sees more than the limits from the auctioneer.
func (a *AuctionRunnerDelegate) ResolveExecutionBackends(logger lager.Logger, traceID string, lrps []auctiontypes.LRPAuction, tasks []auctiontypes.TaskAuction) {
	if len(lrps) == 0 && len(tasks) == 0 {
		return
	}
	if !a.cellsAdvertiseExecutionBackends.Load() {
		return
	}
	logger = logger.Session("resolve-execution-backends")

	deadline := a.clock.NewTimer(a.executionBackendLookupTimeout)
	defer deadline.Stop()

	results := newBackendLookupResults(len(tasks))
	missing := a.rememberedLRPBackends(lrps, results)

	wg := &sync.WaitGroup{}
	expired := false
	launch := func(slots chan struct{}, lookup func()) bool {
		select {
		case slots <- struct{}{}:
		case <-deadline.C():
			return false
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			lookup()
		}()
		return true
	}

	for start := 0; start < len(missing) && !expired; start += desiredLRPLookupBatchSize {
		guids := missing[start:min(start+desiredLRPLookupBatchSize, len(missing))]
		expired = !launch(a.lrpLookups, func() { a.lookUpDesiredLRPs(logger, traceID, guids, results) })
	}
	for i := 0; i < len(tasks) && !expired; i++ {
		taskGuid := tasks[i].TaskGuid
		expired = !launch(a.taskLookups, func() { a.lookUpTask(logger, traceID, i, taskGuid, results) })
	}

	if !expired {
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-deadline.C():
			expired = true
		}
	}

	results.close()
	if expired {
		logger.Info("timed-out", lager.Data{"timeout": a.executionBackendLookupTimeout.String()})
	}

	for i := range lrps {
		lrp := &lrps[i]
		if backend, ok := results.lrpBackends[lrp.ProcessGuid]; ok {
			lrp.ExecutionBackend = backend
			continue
		}
		err, ok := results.lrpFailures[lrp.ProcessGuid]
		if !ok {
			err = errExecutionBackendLookupTimedOut
		}
		lrp.PlacementError = executionBackendLookupFailed + err.Error()
	}

	for i := range tasks {
		task := &tasks[i]
		result := results.tasks[i]
		switch {
		case !result.done:
			task.PlacementError = executionBackendLookupFailed + errExecutionBackendLookupTimedOut.Error()
		case result.err != nil:
			task.PlacementError = executionBackendLookupFailed + result.err.Error()
		default:
			task.ExecutionBackend = result.backend
			task.AppExecutionBackend = result.appBackend
		}
	}
}

// rememberedLRPBackends records the backend of every LRP that the delegate
// remembers, and returns the process guids that it must look up, once each.
func (a *AuctionRunnerDelegate) rememberedLRPBackends(lrps []auctiontypes.LRPAuction, results *backendLookupResults) []string {
	a.lrpBackendsLock.Lock()
	defer a.lrpBackendsLock.Unlock()

	now := a.clock.Now()
	missing := []string{}
	seen := map[string]struct{}{}
	for i := range lrps {
		guid := lrps[i].ProcessGuid
		if _, ok := seen[guid]; ok {
			continue
		}
		seen[guid] = struct{}{}

		if remembered, ok := a.lrpBackends[guid]; ok {
			if now.Before(remembered.expiresAt) {
				results.lrpBackends[guid] = remembered.backend
				continue
			}
			delete(a.lrpBackends, guid)
		}
		missing = append(missing, guid)
	}
	return missing
}

func (a *AuctionRunnerDelegate) rememberLRPBackends(backends map[string]string) {
	a.lrpBackendsLock.Lock()
	defer a.lrpBackendsLock.Unlock()

	// Forgetting every backend at once keeps the cache bounded. The round that
	// asked for these backends reads them from its own results, so it does not
	// depend on the cache keeping them.
	if len(a.lrpBackends)+len(backends) > lrpBackendCacheSize {
		a.lrpBackends = map[string]rememberedBackend{}
	}

	expiresAt := a.clock.Now().Add(lrpBackendCacheTTL)
	for guid, backend := range backends {
		a.lrpBackends[guid] = rememberedBackend{backend: backend, expiresAt: expiresAt}
	}
}

func (a *AuctionRunnerDelegate) lookUpDesiredLRPs(logger lager.Logger, traceID string, guids []string, results *backendLookupResults) {
	desiredLRPs, err := a.bbsClient.DesiredLRPs(logger, traceID, models.DesiredLRPFilter{ProcessGuids: guids})
	if err != nil {
		logger.Error("failed-to-fetch-desired-lrps", err, lager.Data{"process-guids": len(guids)})
		results.recordLRPs(guids, nil, err)
		return
	}

	found := make(map[string]string, len(desiredLRPs))
	for _, desiredLRP := range desiredLRPs {
		found[desiredLRP.ProcessGuid] = rep.ExecutionBackendForDesiredLRP(desiredLRP)
	}
	a.rememberLRPBackends(found)
	results.recordLRPs(guids, found, nil)
}

func (a *AuctionRunnerDelegate) lookUpTask(logger lager.Logger, traceID string, index int, taskGuid string, results *backendLookupResults) {
	task, err := a.bbsClient.TaskByGuid(logger, traceID, taskGuid)
	if err != nil {
		logger.Error("failed-to-fetch-task", err, lager.Data{"task-guid": taskGuid})
		results.recordTask(index, taskLookupResult{err: err})
		return
	}
	results.recordTask(index, taskLookupResult{
		backend:    rep.ExecutionBackendForTask(task),
		appBackend: rep.AppExecutionBackendForTask(task),
	})
}

type taskLookupResult struct {
	done       bool
	backend    string
	appBackend string
	err        error
}

// backendLookupResults collects the answers of one round's lookups. Once the
// round closes it, lookups that finish late leave it alone, so the round can
// read it without the lock.
type backendLookupResults struct {
	lock        sync.Mutex
	closed      bool
	lrpBackends map[string]string
	lrpFailures map[string]error
	tasks       []taskLookupResult
}

func newBackendLookupResults(taskCount int) *backendLookupResults {
	return &backendLookupResults{
		lrpBackends: map[string]string{},
		lrpFailures: map[string]error{},
		tasks:       make([]taskLookupResult, taskCount),
	}
}

func (r *backendLookupResults) recordLRPs(guids []string, found map[string]string, err error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.closed {
		return
	}

	for _, guid := range guids {
		if err != nil {
			r.lrpFailures[guid] = err
			continue
		}
		if backend, ok := found[guid]; ok {
			r.lrpBackends[guid] = backend
			continue
		}
		r.lrpFailures[guid] = errDesiredLRPNotFound
	}
}

func (r *backendLookupResults) recordTask(index int, result taskLookupResult) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.closed {
		return
	}

	result.done = true
	r.tasks[index] = result
}

func (r *backendLookupResults) close() {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.closed = true
}
