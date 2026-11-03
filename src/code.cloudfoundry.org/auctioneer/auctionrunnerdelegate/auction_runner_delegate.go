package auctionrunnerdelegate

import (
	"sync"
	"sync/atomic"
	"time"

	"code.cloudfoundry.org/bbs"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/clock"

	"code.cloudfoundry.org/auction/auctiontypes"
	"code.cloudfoundry.org/lager/v3"
	"code.cloudfoundry.org/rep"
)

type AuctionRunnerDelegate struct {
	repClientFactory models.RepClientFactory
	bbsClient        bbs.InternalClient
	clock            clock.Clock

	executionBackendLookupTimeout time.Duration

	// cellsAdvertiseExecutionBackends records whether any cell presence in the
	// latest fetch advertised an execution backend policy.
	cellsAdvertiseExecutionBackends atomic.Bool

	lrpBackendsLock sync.Mutex
	lrpBackends     map[string]rememberedBackend

	lrpLookups  chan struct{}
	taskLookups chan struct{}
}

// New returns a delegate. executionBackendLookupTimeout bounds how long one
// auction round waits to learn which backend its workloads select, and a
// value that is not positive means DefaultExecutionBackendLookupTimeout.
func New(
	repClientFactory models.RepClientFactory,
	bbsClient bbs.InternalClient,
	clock clock.Clock,
	executionBackendLookupTimeout time.Duration,
) *AuctionRunnerDelegate {
	if executionBackendLookupTimeout <= 0 {
		executionBackendLookupTimeout = DefaultExecutionBackendLookupTimeout
	}
	return &AuctionRunnerDelegate{
		repClientFactory:              repClientFactory,
		bbsClient:                     bbsClient,
		clock:                         clock,
		executionBackendLookupTimeout: executionBackendLookupTimeout,
		lrpBackends:                   map[string]rememberedBackend{},
		lrpLookups:                    make(chan struct{}, lrpLookupConcurrency),
		taskLookups:                   make(chan struct{}, taskLookupConcurrency),
	}
}

func (a *AuctionRunnerDelegate) FetchCellReps(logger lager.Logger, traceID string) (map[string]models.RepClient, error) {
	cells, err := a.bbsClient.Cells(logger, traceID)
	cellReps := map[string]models.RepClient{}
	if err != nil {
		return cellReps, err
	}

	// The presences include cells that are evacuating and cells whose state
	// the auction cannot fetch, which the zones leave out, so a VM workload is
	// still resolved, and not placed on a cell that refuses it, when those are
	// the only cells that offer its backend.
	advertise := false
	for _, cell := range cells {
		for _, tag := range cell.OptionalPlacementTags {
			if rep.IsExecutionBackendPlacementTag(tag) {
				advertise = true
				break
			}
		}
	}
	a.cellsAdvertiseExecutionBackends.Store(advertise)

	for _, cell := range cells {
		client, err := a.repClientFactory.CreateClient(cell.RepAddress, cell.RepUrl, traceID)
		if err != nil {
			logger.Error("create-rep-client-failed", err)
			continue
		}
		cellReps[cell.CellId] = client
	}

	return cellReps, nil
}

func (a *AuctionRunnerDelegate) AuctionCompleted(logger lager.Logger, traceID string, results auctiontypes.AuctionResults) {
	for i := range results.FailedTasks {
		task := &results.FailedTasks[i]
		err := a.bbsClient.RejectTask(logger, traceID, task.TaskGuid, task.PlacementError)
		if err != nil {
			logger.Error("failed-to-reject-task", err, lager.Data{
				"task":           task,
				"auction-result": "failed",
			})
		}
	}

	for i := range results.FailedLRPs {
		lrp := &results.FailedLRPs[i]
		err := a.bbsClient.FailActualLRP(logger, traceID, &lrp.ActualLRPKey, lrp.PlacementError)
		if err != nil {
			logger.Error("failed-to-fail-LRP", err, lager.Data{
				"lrp":            lrp,
				"auction-result": "failed",
			})
		}
	}
}
