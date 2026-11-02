package auctiontypes

import (
	"errors"
	"strings"
	"time"

	bbsmodels "code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/lager/v3"
	"code.cloudfoundry.org/rep"
	"github.com/tedsuo/ifrit"
)

// Auction Runners

var ErrorCellMismatch = errors.New("found no compatible cell for required rootfs")
var ErrorVolumeDriverMismatch = errors.New("found no compatible cell with required volume drivers")

type PlacementTagMismatchError struct {
	tags []string
}

func NewPlacementTagMismatchError(tags []string) error {
	return PlacementTagMismatchError{tags: tags}
}

func (e PlacementTagMismatchError) Error() string {
	switch len(e.tags) {
	case 0:
		return "found no compatible cell with no placement tags"
	case 1:
		return "found no compatible cell with placement tag \"" + e.tags[0] + "\""
	default:
		count := len(e.tags) - 1
		lastTag := e.tags[count]
		tags := make([]string, count)
		for i, tag := range e.tags[:count] {
			tags[i] = "\"" + tag + "\""
		}
		return "found no compatible cell with placement tags " + strings.Join(tags, ", ") + " and \"" + lastTag + "\""
	}
}

// ExecutionBackendMismatchError is the placement error for a workload whose
// execution backend no cell offers or accepts, among the cells that match its
// rootfs, volume drivers, and placement tags. Required lists the backends that
// the cells which offer the backend, but do not accept it, require.
type ExecutionBackendMismatchError struct {
	Backend  string
	Required []string
}

func (e ExecutionBackendMismatchError) Error() string {
	if len(e.Required) == 0 {
		return "found no compatible cell offering execution backend \"" + e.Backend + "\""
	}
	return "found no compatible cell accepting execution backend \"" + e.Backend + "\", the cells that offer it require " + rep.QuoteExecutionBackends(e.Required)
}

var ErrorNothingToStop = errors.New("nothing to stop")
var ErrorCellCommunication = errors.New("unable to communicate to compatible cells")
var ErrorExceededInflightCreation = errors.New("waiting to start instance: reached in-flight start limit")

//go:generate counterfeiter -o fakes/fake_auction_runner.go . AuctionRunner
type AuctionRunner interface {
	ifrit.Runner
	ScheduleLRPsForAuctions([]bbsmodels.LRPStartRequest, string)
	ScheduleTasksForAuctions([]bbsmodels.TaskStartRequest, string)
}

type AuctionRunnerDelegate interface {
	FetchCellReps(lager.Logger, string) (map[string]bbsmodels.RepClient, error)
	AuctionCompleted(lager.Logger, string, AuctionResults)

	// ResolveExecutionBackends sets the ExecutionBackend of every auction to
	// the backend that its workload selects, and the AppExecutionBackend of
	// every task auction. When it cannot tell in time, it sets the auction's
	// PlacementError instead, and the auction is not scheduled in this round.
	ResolveExecutionBackends(logger lager.Logger, traceID string, lrps []LRPAuction, tasks []TaskAuction)
}

//go:generate counterfeiter -o fakes/fake_metric_emitter.go . AuctionMetricEmitterDelegate
type AuctionMetricEmitterDelegate interface {
	FetchStatesCompleted(time.Duration) error
	FailedCellStateRequest() error
	AuctionCompleted(AuctionResults) error
}

type AuctionRequest struct {
	LRPs  []LRPAuction
	Tasks []TaskAuction
}

type AuctionResults struct {
	SuccessfulLRPs  []LRPAuction
	SuccessfulTasks []TaskAuction
	FailedLRPs      []LRPAuction
	FailedTasks     []TaskAuction
}

// LRPStart and Task Auctions

type AuctionRecord struct {
	Winner   string
	Attempts int

	QueueTime    time.Time
	WaitDuration time.Duration

	PlacementError string
}

func NewAuctionRecord(now time.Time) AuctionRecord {
	return AuctionRecord{QueueTime: now}
}

// LRPAuction is one LRP instance to place. ExecutionBackend is the backend
// that the LRP selects, which the auctioneer looks up because BBS does not
// send it, and it is empty when the LRP selects none.
type LRPAuction struct {
	bbsmodels.SchedulingLRP
	AuctionRecord
	ExecutionBackend string
}

func NewLRPAuction(lrp bbsmodels.SchedulingLRP, now time.Time) LRPAuction {
	return LRPAuction{
		SchedulingLRP: lrp,
		AuctionRecord: NewAuctionRecord(now),
	}
}

func (a *LRPAuction) Copy() LRPAuction {
	return LRPAuction{a.SchedulingLRP.Copy(), a.AuctionRecord, a.ExecutionBackend}
}

// TaskAuction is one task to place. ExecutionBackend has the same meaning as
// it does for an LRPAuction. AppExecutionBackend is the backend that the app a
// staging task builds selects, which a cell checks for a task in a domain
// that is exempt from its requirement.
type TaskAuction struct {
	bbsmodels.SchedulingTask
	AuctionRecord
	ExecutionBackend    string
	AppExecutionBackend string
}

func NewTaskAuction(task bbsmodels.SchedulingTask, now time.Time) TaskAuction {
	return TaskAuction{
		SchedulingTask: task,
		AuctionRecord:  NewAuctionRecord(now),
	}
}

func (a *TaskAuction) Copy() TaskAuction {
	return TaskAuction{a.SchedulingTask.Copy(), a.AuctionRecord, a.ExecutionBackend, a.AppExecutionBackend}
}
