package fluxmonitorv2

import (
	"fmt"
	"math/big"
	"reflect"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/pkg/errors"
	"github.com/shopspring/decimal"
	"github.com/smartcontractkit/chainlink/core/assets"
	"github.com/smartcontractkit/chainlink/core/internal/gethwrappers/generated/flags_wrapper"
	"github.com/smartcontractkit/chainlink/core/internal/gethwrappers/generated/flux_aggregator_wrapper"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services/eth"
	"github.com/smartcontractkit/chainlink/core/services/eth/contracts"
	"github.com/smartcontractkit/chainlink/core/services/log"
	"github.com/smartcontractkit/chainlink/core/store/models"
	"github.com/smartcontractkit/chainlink/core/utils"
	"github.com/tevino/abool"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

const hibernationPollPeriod = 24 * time.Hour

// Specification defines the Flux Monitor specification
type Specification struct {
	ID                string
	JobID             int32
	ContractAddress   models.EIP55Address
	Precision         int32
	Threshold         float32
	AbsoluteThreshold float32
	PollTimerPeriod   time.Duration
	PollTimerDisabled bool
	IdleTimerPeriod   time.Duration
	IdleTimerDisabled bool
}

// FluxMonitorFactory holds the New method needed to create a new instance
// of a DeviationChecker.
type FluxMonitorFactory interface {
	New(Specification, PipelineRun, *assets.Link, Config) (*FluxMonitor, error)
}

type fluxMonitorFactory struct {
	db             *Database
	ethClient      eth.Client
	logBroadcaster log.Broadcaster
}

// New constructs an instance of PollingDeviationChecker with sane defaults and
// validation.
func (f fluxMonitorFactory) New(
	spec Specification,
	pipelineRun PipelineRun,
	minJobPayment *assets.Link,
	cfg Config,
) (*FluxMonitor, error) {
	// Validate the poll timer
	if !spec.PollTimerDisabled &&
		spec.PollTimerPeriod < cfg.MinimumPollingInterval() {

		return nil, fmt.Errorf(
			"pollTimerPeriod must be equal or greater than %s",
			cfg.MinimumPollingInterval(),
		)
	}

	// Set up the flux aggregator
	f.logBroadcaster.AddDependents(1)
	fluxAggregator, err := flux_aggregator_wrapper.NewFluxAggregator(
		spec.ContractAddress.Address(),
		f.ethClient,
	)
	if err != nil {
		return nil, err
	}

	// Set up the contract flags
	var flagsContract *contracts.Flags
	if cfg.FlagsContractAddress != "" {
		flagsContractAddress := common.HexToAddress(cfg.FlagsContractAddress)
		flagsContract, err = contracts.NewFlagsContract(flagsContractAddress, f.ethClient)
		logger.ErrorIf(
			err,
			fmt.Sprintf(
				"unable to create Flags contract instance, check address: %s",
				cfg.FlagsContractAddress,
			),
		)
	}

	min, err := fluxAggregator.MinSubmissionValue(nil)
	if err != nil {
		return nil, err
	}

	max, err := fluxAggregator.MaxSubmissionValue(nil)
	if err != nil {
		return nil, err
	}

	return NewFluxMonitor(
		pipelineRun,
		cfg,
		f.db,
		fluxAggregator,
		f.logBroadcaster,
		spec,
		f.ethClient,
		minJobPayment,
		flagsContract,
		func() { f.logBroadcaster.DependentReady() },
		min,
		max,
	)
}

// FluxMonitor polls external price adapters via HTTP to check for price swings.
type FluxMonitor struct {
	cfg         Config
	pipelineRun PipelineRun
	spec        Specification

	fluxAggregator flux_aggregator_wrapper.FluxAggregatorInterface
	logBroadcaster log.Broadcaster
	flagsContract  *contracts.Flags
	oracleAddress  common.Address

	db            *Database
	ethClient     eth.Client
	minJobPayment *assets.Link

	isHibernating    bool
	connected        *abool.AtomicBool
	backlog          *utils.BoundedPriorityQueue
	chProcessLogs    chan struct{}
	pollTicker       utils.PausableTicker
	hibernationTimer utils.ResettableTimer
	idleTimer        utils.ResettableTimer
	roundTimer       utils.ResettableTimer

	minSubmission, maxSubmission *big.Int

	readyForLogs func()
	chStop       chan struct{}
	waitOnStop   chan struct{}
}

// NewFluxMonitor returns a new instance of PollingDeviationChecker.
func NewFluxMonitor(
	pipelineRun PipelineRun,
	cfg Config,

	db *Database,
	fluxAggregator flux_aggregator_wrapper.FluxAggregatorInterface,
	logBroadcaster log.Broadcaster,
	spec Specification,
	ethClient eth.Client,
	minJobPayment *assets.Link,
	flagsContract *contracts.Flags,
	readyForLogs func(),
	minSubmission, maxSubmission *big.Int,
) (*FluxMonitor, error) {
	return &FluxMonitor{
		pipelineRun: pipelineRun,
		cfg:         cfg,

		readyForLogs:     readyForLogs,
		db:               db,
		logBroadcaster:   logBroadcaster,
		fluxAggregator:   fluxAggregator,
		flagsContract:    flagsContract,
		spec:             spec,
		ethClient:        ethClient,
		minJobPayment:    minJobPayment,
		pollTicker:       utils.NewPausableTicker(spec.PollTimerPeriod),
		hibernationTimer: utils.NewResettableTimer(),
		idleTimer:        utils.NewResettableTimer(),
		roundTimer:       utils.NewResettableTimer(),
		minSubmission:    minSubmission,
		maxSubmission:    maxSubmission,
		isHibernating:    false,
		connected:        abool.New(),
		backlog: utils.NewBoundedPriorityQueue(map[uint]uint{
			// We want reconnecting nodes to be able to submit to a round
			// that hasn't hit maxAnswers yet, as well as the newest round.
			PriorityNewRoundLog:      2,
			PriorityAnswerUpdatedLog: 1,
			PriorityFlagChangedLog:   2,
		}),
		chProcessLogs: make(chan struct{}, 1),
		chStop:        make(chan struct{}),
		waitOnStop:    make(chan struct{}),
	}, nil
}

const (
	PriorityFlagChangedLog   uint = 0
	PriorityNewRoundLog      uint = 1
	PriorityAnswerUpdatedLog uint = 2
)

// Start implements the job.Service interface. It begins the CSP consumer in a
// single goroutine to poll the price adapters and listen to NewRound events.
//
// INVESTIGATE - Does this return an error?
func (fm *FluxMonitor) Start() error {
	logger.Debugw("Starting checker for job",
		"job", fm.spec.JobID,
		"initr", fm.spec.ID,
	)

	go fm.consume()

	return nil
}

func (fm *FluxMonitor) setIsHibernatingStatus() {
	if fm.flagsContract == nil {
		fm.isHibernating = false

		return
	}

	isFlagLowered, err := fm.isFlagLowered()
	if err != nil {
		logger.Errorf("unable to set hibernation status: %v", err)

		fm.isHibernating = false
	} else {
		fm.isHibernating = !isFlagLowered
	}
}

func (fm *FluxMonitor) isFlagLowered() (bool, error) {
	if fm.flagsContract == nil {
		return true, nil
	}

	flags, err := fm.flagsContract.GetFlags(nil,
		[]common.Address{utils.ZeroAddress, fm.spec.ContractAddress.Address()},
	)
	if err != nil {
		return true, err
	}
	return !flags[0] || !flags[1], nil
}

// Close implements the job.Service interface. It stops this instance from
// polling, cleaning up resources.
//
// INVESTIGATE - Does this error return work?
func (fm *FluxMonitor) Close() error {
	fm.pollTicker.Destroy()
	fm.hibernationTimer.Stop()
	fm.idleTimer.Stop()
	fm.roundTimer.Stop()
	close(fm.chStop)
	<-fm.waitOnStop

	return nil
}

// OnConnect sets the poller as connected
func (fm *FluxMonitor) OnConnect() {
	logger.Debugw("PollingDeviationChecker connected to Ethereum node",
		"jobID", fm.spec.JobID,
		"address", fm.spec.ContractAddress.Hex(),
	)

	fm.connected.Set()
}

// OnDisconnect sets the poller as disconnected
func (fm *FluxMonitor) OnDisconnect() {
	logger.Debugw("PollingDeviationChecker disconnected from Ethereum node",
		"jobID", fm.spec.JobID,
		"address", fm.spec.ContractAddress.Hex(),
	)

	fm.connected.UnSet()
}

// JobID returns the v1 job id
// INVESTIGATE - Not sure if this is still needed?
// This is being used in the log broadcaster but the broadcaster also checks
// whether it is a v2 job.
func (p *FluxMonitor) JobID() models.JobID {
	return models.NewJobID()
}

// JobIDV2 returns the the v2 job id
func (p *FluxMonitor) JobIDV2() int32 { return p.spec.JobID }

// IsV2Job determines whether this is a v2 job
func (p *FluxMonitor) IsV2Job() bool { return true }

// HandleLog processes the contract logs
func (p *FluxMonitor) HandleLog(broadcast log.Broadcast, err error) {
	if err != nil {
		logger.Errorf("got error from LogBroadcaster: %v", err)
		return
	}

	log := broadcast.DecodedLog()
	if log == nil || reflect.ValueOf(log).IsNil() {
		logger.Error("HandleLog: ignoring nil value")
		return
	}

	switch log := log.(type) {
	case *flux_aggregator_wrapper.FluxAggregatorNewRound:
		p.backlog.Add(PriorityNewRoundLog, broadcast)

	case *flux_aggregator_wrapper.FluxAggregatorAnswerUpdated:
		p.backlog.Add(PriorityAnswerUpdatedLog, broadcast)

	case *flags_wrapper.FlagsFlagRaised:
		if log.Subject == utils.ZeroAddress || log.Subject == p.spec.ContractAddress.Address() {
			p.backlog.Add(PriorityFlagChangedLog, broadcast)
		}

	case *flags_wrapper.FlagsFlagLowered:
		if log.Subject == utils.ZeroAddress || log.Subject == p.spec.ContractAddress.Address() {
			p.backlog.Add(PriorityFlagChangedLog, broadcast)
		}

	default:
		logger.Warnf("unexpected log type %T", log)
		return
	}

	select {
	case p.chProcessLogs <- struct{}{}:
	default:
	}
}

func (p *FluxMonitor) consume() {
	defer close(p.waitOnStop)

	if err := p.SetOracleAddress(); err != nil {
		logger.Warnw(
			"unable to set oracle address, this flux monitor job may not work correctly",
			"err",
			err,
		)
	}

	// Subscribe to contract logs
	isConnected := false
	fluxAggLogListener, err := newFluxAggregatorDecodingLogListener(
		p.fluxAggregator.Address(),
		p.ethClient,
		p,
	)
	if err != nil {
		logger.Errorw("unable to create flux agg decoding log listener", "err", err)
		return
	}

	isConnected = p.logBroadcaster.Register(p.fluxAggregator.Address(), fluxAggLogListener)
	defer func() { p.logBroadcaster.Unregister(p.fluxAggregator.Address(), fluxAggLogListener) }()

	if p.flagsContract != nil {
		flagsLogListener := contracts.NewFlagsDecodingLogListener(p.flagsContract, p)
		flagsConnected := p.logBroadcaster.Register(p.flagsContract.Address, flagsLogListener)
		isConnected = isConnected && flagsConnected
		defer func() { p.logBroadcaster.Unregister(p.flagsContract.Address, flagsLogListener) }()
	}

	if isConnected {
		p.connected.Set()
	} else {
		p.connected.UnSet()
	}

	p.readyForLogs()
	p.setIsHibernatingStatus()
	p.setInitialTickers()
	p.performInitialPoll()

	for {
		select {
		case <-p.chStop:
			return

		case <-p.chProcessLogs:
			p.processLogs()

		case <-p.pollTicker.Ticks():
			logger.Debugw("Poll ticker fired",
				"pollPeriod", p.spec.PollTimerPeriod, // TODO - Make this a readable string
				"idleDuration", p.spec.IdleTimerPeriod, // TODO - Make this a readable string
				"contract", p.spec.ContractAddress.Hex(),
			)
			p.pollIfEligible(DeviationThresholds{
				Rel: float64(p.spec.Threshold),
				Abs: float64(p.spec.AbsoluteThreshold),
			})

		case <-p.idleTimer.Ticks():
			logger.Debugw("Idle ticker fired",
				"pollPeriod", p.spec.PollTimerPeriod,
				"idleDuration", p.spec.IdleTimerPeriod,
				"contract", p.spec.ContractAddress.Hex(),
			)
			p.pollIfEligible(DeviationThresholds{Rel: 0, Abs: 0})

		case <-p.roundTimer.Ticks():
			logger.Debugw("Round timeout ticker fired",
				"pollPeriod", p.spec.PollTimerPeriod,
				"idleDuration", p.spec.IdleTimerPeriod,
				"contract", p.spec.ContractAddress.Hex(),
			)
			p.pollIfEligible(DeviationThresholds{
				Rel: float64(p.spec.Threshold),
				Abs: float64(p.spec.AbsoluteThreshold),
			})

		case <-p.hibernationTimer.Ticks():
			p.pollIfEligible(DeviationThresholds{Rel: 0, Abs: 0})
		}
	}
}

func (p *FluxMonitor) SetOracleAddress() error {
	oracleAddrs, err := p.fluxAggregator.GetOracles(nil)
	if err != nil {
		return errors.Wrap(err, "failed to get list of oracles from FluxAggregator contract")
	}
	accounts := p.db.KeyStoreAccounts()
	for _, acct := range accounts {
		for _, oracleAddr := range oracleAddrs {
			if acct.Address == oracleAddr {
				p.oracleAddress = oracleAddr
				return nil
			}
		}
	}
	if len(accounts) > 0 {
		addr := accounts[0].Address
		logger.Warnw("None of the node's keys matched any oracle addresses, using first available key. This flux monitor job may not work correctly", "address", addr)
		p.oracleAddress = addr
	} else {
		logger.Error("No keys found. This flux monitor job may not work correctly")
	}
	return errors.New("none of the node's keys matched any oracle addresses")

}

func (p *FluxMonitor) performInitialPoll() {
	if p.shouldPerformInitialPoll() {
		p.pollIfEligible(DeviationThresholds{
			Rel: float64(p.spec.Threshold),
			Abs: float64(p.spec.AbsoluteThreshold),
		})
	}
}

func (p *FluxMonitor) shouldPerformInitialPoll() bool {
	return !(p.spec.PollTimerDisabled && p.spec.IdleTimerDisabled || p.isHibernating)
}

// hibernate restarts the PollingDeviationChecker in hibernation mode
func (p *FluxMonitor) hibernate() {
	logger.Infof("entering hibernation mode for contract: %s", p.spec.ContractAddress.Hex())
	p.isHibernating = true
	p.resetTickers(flux_aggregator_wrapper.OracleRoundState{})
}

// reactivate restarts the PollingDeviationChecker without hibernation mode
func (p *FluxMonitor) reactivate() {
	logger.Infof("exiting hibernation mode, reactivating contract: %s", p.spec.ContractAddress.Hex())
	p.isHibernating = false
	p.setInitialTickers()
	p.pollIfEligible(DeviationThresholds{Rel: 0, Abs: 0})
}

func (p *FluxMonitor) processLogs() {
	for !p.backlog.Empty() {
		maybeBroadcast := p.backlog.Take()
		broadcast, ok := maybeBroadcast.(log.Broadcast)
		if !ok {
			logger.Errorf("Failed to convert backlog into LogBroadcast.  Type is %T", maybeBroadcast)
		}

		// If the log is a duplicate of one we've seen before, ignore it (this
		// happens because of the LogBroadcaster's backfilling behavior).
		consumed, err := broadcast.WasAlreadyConsumed()
		if err != nil {
			logger.Errorf("Error determining if log was already consumed: %v", err)
			continue
		} else if consumed {
			logger.Debug("Log was already consumed by Flux Monitor, skipping")
			continue
		}

		switch log := broadcast.DecodedLog().(type) {
		case *flux_aggregator_wrapper.FluxAggregatorNewRound:
			p.respondToNewRoundLog(*log)
			err = broadcast.MarkConsumed()
			if err != nil {
				logger.Errorf("Error marking log as consumed: %v", err)
			}

		case *flux_aggregator_wrapper.FluxAggregatorAnswerUpdated:
			p.respondToAnswerUpdatedLog(*log)
			err = broadcast.MarkConsumed()
			if err != nil {
				logger.Errorf("Error marking log as consumed: %v", err)
			}

		case *flags_wrapper.FlagsFlagRaised:
			// check the contract before hibernating, because one flag could be lowered
			// while the other flag remains raised
			var isFlagLowered bool
			isFlagLowered, err = p.isFlagLowered()
			logger.ErrorIf(err, "Error determining if flag is still raised")
			if !isFlagLowered {
				p.hibernate()
			}
			err = broadcast.MarkConsumed()
			logger.ErrorIf(err, "Error marking log as consumed")

		case *flags_wrapper.FlagsFlagLowered:
			p.reactivate()
			err = broadcast.MarkConsumed()
			logger.ErrorIf(err, "Error marking log as consumed")

		default:
			logger.Errorf("unknown log %v of type %T", log, log)
		}
	}
}

// The AnswerUpdated log tells us that round has successfully closed with a new
// answer.  We update our view of the oracleRoundState in case this log was
// generated by a chain reorg.
func (p *FluxMonitor) respondToAnswerUpdatedLog(log flux_aggregator_wrapper.FluxAggregatorAnswerUpdated) {
	logger.Debugw("AnswerUpdated log", p.loggerFieldsForAnswerUpdated(log)...)

	roundState, err := p.roundState(0)
	if err != nil {
		logger.Errorw(fmt.Sprintf("could not fetch oracleRoundState: %v", err), p.loggerFieldsForAnswerUpdated(log)...)
		return
	}
	p.resetTickers(roundState)
}

// The NewRound log tells us that an oracle has initiated a new round.  This tells us that we
// need to poll and submit an answer to the contract regardless of the deviation.
func (p *FluxMonitor) respondToNewRoundLog(log flux_aggregator_wrapper.FluxAggregatorNewRound) {
	logger.Debugw("NewRound log", p.loggerFieldsForNewRound(log)...)

	promSetBigInt(promFMSeenRound.WithLabelValues(fmt.Sprintf("%d", p.spec.JobID)), log.RoundId)

	//
	// NewRound answer submission logic:
	//   - Any log that reaches this point, regardless of chain reorgs or log backfilling, is one that we have
	//         not seen before.  Therefore, we should consider acting upon it.
	//   - We always take the round ID from the log, rather than the round ID suggested by `.RoundState`.  The
	//         reason is that if two NewRound logs come in in rapid succession, and we submit a tx for the first,
	//         the `.ReportableRoundID` field in the roundState() response for the 2nd log will not reflect the
	//         fact that we've submitted for the first round (assuming it hasn't been mined yet).
	//   - In the event of a reorg that pushes our previous submissions back into the mempool, we can rely on the
	//         TxManager to ensure they end up being mined into blocks, but this may cause them to revert if they
	//         are mined in an order that violates certain conditions in the FluxAggregator (restartDelay, etc.).
	//         Therefore, the cleanest solution at present is to resubmit for the reorged rounds.  The drawback
	//         of this approach is that one or the other submission tx for a given round will revert, costing the
	//         node operator some gas.  The benefit is that those submissions are guaranteed to be made, ensuring
	//         that we have high data availability (and also ensuring that node operators get paid).
	//   - There are a few straightforward cases where we don't want to submit:
	//         - When we're not eligible
	//         - When the aggregator is underfunded
	//         - When we were the initiator of the round (i.e. we've received our own NewRound log)
	//   - There are a few more nuanced cases as well:
	//         - When our node polls at the same time as another node, and both attempt to start a round.  In that
	//               case, it's possible that the other node will start the round, and our node will see the NewRound
	//               log and try to submit again.
	//         - When the poll ticker fires very soon after we've responded to a NewRound log.
	//
	//         To handle these more nuanced cases, we record round IDs and whether we've submitted for those rounds
	//         in the DB.  If we see we've already submitted for a given round, we simply bail out.
	//
	//         However, in the case of a chain reorganization, we might see logs with round IDs that we've already
	//         seen.  As mentioned above, we want to re-respond to these rounds to ensure high data availability.
	//         Therefore, if a log arrives with a round ID that is < the most recent that we submitted to, we delete
	//         all of the round IDs in the DB back to (and including) the incoming round ID.  This essentially
	//         rewinds the system back to a state wherein those reorg'ed rounds never occurred, allowing it to move
	//         forward normally.
	//
	//         There is one small exception: if the reorg is fairly shallow, and only un-starts a single round, we
	//         do not need to resubmit, because the TxManager will ensure that our existing submission gets back
	//         into the chain.  There is a very small risk that one of the nodes in the quorum (namely, whichever
	//         one started the previous round) will have its existing submission mined first, thereby violating
	//         the restartDelay, but as this risk is isolated to a single node, the round will not time out and
	//         go stale.  We consider this acceptable.
	//

	logRoundID := uint32(log.RoundId.Uint64())

	// We always want to reset the idle timer upon receiving a NewRound log, so we do it before any `return` statements.
	p.resetIdleTimer(log.StartedAt.Uint64())

	mostRecentRoundID, err := p.db.MostRecentFluxMonitorRoundID(p.spec.ContractAddress.Address())
	if err != nil && err != gorm.ErrRecordNotFound {
		logger.Errorw(fmt.Sprintf("error fetching Flux Monitor most recent round ID from DB: %v", err), p.loggerFieldsForNewRound(log)...)
		return
	}

	if logRoundID < mostRecentRoundID {
		err = p.db.DeleteFluxMonitorRoundsBackThrough(p.spec.ContractAddress.Address(), logRoundID)
		if err != nil {
			logger.Errorw(fmt.Sprintf("error deleting reorged Flux Monitor rounds from DB: %v", err), p.loggerFieldsForNewRound(log)...)
			return
		}
	}

	roundStats, jobRunStatus, err := p.statsAndStatusForRound(logRoundID)
	if err != nil {
		logger.Errorw(fmt.Sprintf("error determining round stats / run status for round: %v", err), p.loggerFieldsForNewRound(log)...)
		return
	}

	if roundStats.NumSubmissions > 0 {
		// This indicates either that:
		//     - We tried to start a round at the same time as another node, and their transaction was mined first, or
		//     - The chain experienced a shallow reorg that unstarted the current round.
		// If our previous attempt is still pending, return early and don't re-submit
		// If our previous attempt is already over (completed or errored), we should retry
		if !jobRunStatus.Finished() {
			logger.Debugw("Ignoring new round request: started round simultaneously with another node", p.loggerFieldsForNewRound(log)...)
			return
		}
	}

	// Ignore rounds we started
	if p.oracleAddress == log.StartedBy {
		logger.Infow("Ignoring new round request: we started this round", p.loggerFieldsForNewRound(log)...)
		return
	}

	// Ignore rounds we're not eligible for, or for which we won't be paid
	roundState, err := p.roundState(logRoundID)
	if err != nil {
		logger.Errorw(fmt.Sprintf("Ignoring new round request: error fetching eligibility from contract: %v", err), p.loggerFieldsForNewRound(log)...)
		return
	}
	p.resetTickers(roundState)
	err = p.checkEligibilityAndAggregatorFunding(roundState)
	if err != nil {
		logger.Infow(fmt.Sprintf("Ignoring new round request: %v", err), p.loggerFieldsForNewRound(log)...)
		return
	}

	logger.Infow("Responding to new round request", p.loggerFieldsForNewRound(log)...)

	// request, err := models.MarshalToMap(&roundState)
	// if err != nil {
	// 	logger.Warnw("Error marshalling roundState for request meta", p.loggerFieldsForNewRound(log)...)
	// 	return
	// }

	// ctx, cancel := utils.CombinedContext(p.chStop)
	// defer cancel()
	// polledAnswer, err := p.fetcher.Fetch(ctx, request)
	// if err != nil {
	// 	logger.Errorw(fmt.Sprintf("unable to fetch median price: %v", err), p.loggerFieldsForNewRound(log)...)
	// 	return
	// }

	// if !p.isValidSubmission(logger.Default.SugaredLogger, polledAnswer) {
	// 	return
	// }

	// var payment assets.Link
	// if roundState.PaymentAmount == nil {
	// 	logger.Error("roundState.PaymentAmount shouldn't be nil")
	// } else {
	// 	payment = assets.Link(*roundState.PaymentAmount)
	// }

	// err = p.createJobRun(polledAnswer, logRoundID, &payment)
	// if err != nil {
	// 	logger.Errorw(fmt.Sprintf("unable to create job run: %v", err), p.loggerFieldsForNewRound(log)...)
	// 	return
	// }
}

var (
	// ErrNotEligible defines when the round is not eligible for submission
	ErrNotEligible = errors.New("not eligible to submit")
	// ErrUnderfunded defines when the aggregator does not have sufficient funds
	ErrUnderfunded = errors.New("aggregator is underfunded")
	// ErrPaymentTooLow defines when the round payment is too low
	ErrPaymentTooLow = errors.New("round payment amount < minimum contract payment")
)

func (p *FluxMonitor) checkEligibilityAndAggregatorFunding(roundState flux_aggregator_wrapper.OracleRoundState) error {
	if !roundState.EligibleToSubmit {
		return ErrNotEligible
	} else if !p.sufficientFunds(roundState) {
		return ErrUnderfunded
	} else if !p.sufficientPayment(roundState.PaymentAmount) {
		return ErrPaymentTooLow
	}
	return nil
}

// MinFundedRounds defines the minimum number of rounds that needs to be paid
// to oracles on a contract
const MinFundedRounds int64 = 3

// sufficientFunds checks if the contract has sufficient funding to pay all the
// oracles on a contract for a minimum number of rounds, based on the payment
// amount in the contract
func (fm *FluxMonitor) sufficientFunds(state flux_aggregator_wrapper.OracleRoundState) bool {
	min := big.NewInt(int64(state.OracleCount))
	min = min.Mul(min, big.NewInt(MinFundedRounds))
	min = min.Mul(min, state.PaymentAmount)
	return state.AvailableFunds.Cmp(min) >= 0
}

// sufficientPayment checks if the available payment is enough to submit an answer. It compares
// the payment amount on chain with the min payment amount listed in the job spec / ENV var.
func (fm *FluxMonitor) sufficientPayment(payment *big.Int) bool {
	aboveOrEqMinGlobalPayment := payment.Cmp(fm.cfg.MinContractPayment.ToInt()) >= 0
	aboveOrEqMinJobPayment := true
	if fm.minJobPayment != nil {
		aboveOrEqMinJobPayment = payment.Cmp(fm.minJobPayment.ToInt()) >= 0
	}
	return aboveOrEqMinGlobalPayment && aboveOrEqMinJobPayment
}

// DeviationThresholds carries parameters used by the threshold-trigger logic
type DeviationThresholds struct {
	Rel float64 // Relative change required, i.e. |new-old|/|old| >= Rel
	Abs float64 // Absolute change required, i.e. |new-old| >= Abs
}

func (fm *FluxMonitor) pollIfEligible(thresholds DeviationThresholds) {
	l := logger.Default.With(
		"jobID", fm.spec.JobID,
		"address", fm.spec.ContractAddress.Hex(),
		"threshold", thresholds.Rel,
		"absoluteThreshold", thresholds.Abs,
	)

	if !fm.connected.IsSet() {
		l.Warnw("not connected to Ethereum node, skipping poll")

		return
	}

	//
	// Poll ticker submission logic:
	//   - We avoid saving on-chain state wherever possible.  Therefore, we do not know which round we should be
	//         submitting for when the pollTicker fires.
	//   - We pass 0 into `roundState()`, and the FluxAggregator returns a suggested roundID for us to
	//         submit to, as well as our eligibility to submit to that round.
	//   - If the poll ticker fires very soon after we've responded to a NewRound log, and our tx has not been
	//         mined, we risk double-submitting for a round.  To detect this, we check the DB to see whether
	//         we've responded to this round already, and bail out if so.
	//

	// Ask the FluxAggregator which round we should be submitting to, and what the state of that round is.
	roundState, err := fm.roundState(0)
	if err != nil {
		l.Errorw("unable to determine eligibility to submit from FluxAggregator contract", "err", err)
		fm.db.RecordError(
			fm.spec.JobID,
			"Unable to call roundState method on provided contract. Check contract address.",
		)

		return
	}

	fm.resetTickers(roundState)
	l = l.With("reportableRound", roundState.RoundId)

	roundStats, jobRunStatus, err := fm.statsAndStatusForRound(roundState.RoundId)
	if err != nil {
		l.Errorw("error determining round stats / run status for round", "err", err)

		return
	}

	// If we've already successfully submitted to this round (ie through a NewRound log)
	// and the associated JobRun hasn't errored, skip polling
	if roundStats.NumSubmissions > 0 && !jobRunStatus.Errored() {
		l.Infow("skipping poll: round already answered, tx unconfirmed")

		return
	}

	// Don't submit if we're not eligible, or won't get paid
	err = fm.checkEligibilityAndAggregatorFunding(roundState)
	if err != nil {
		l.Infow(fmt.Sprintf("skipping poll: %v", err))

		return
	}

	// Call the v2 pipeline to execute a new job run
	polledAnswer, err := fm.pipelineRun.Execute()
	if err != nil {
		l.Errorw("can't fetch answer", "err", err)
		fm.db.RecordError(fm.spec.JobID, "Error polling")

		return
	}

	if !fm.isValidSubmission(l, *polledAnswer) {
		return
	}

	jobID := fmt.Sprintf("%d", fm.spec.JobID)
	latestAnswer := decimal.NewFromBigInt(roundState.LatestSubmission, -fm.spec.Precision)
	promSetDecimal(promFMSeenValue.WithLabelValues(jobID), *polledAnswer)

	l = l.With(
		"latestAnswer", latestAnswer,
		"polledAnswer", polledAnswer,
	)

	if roundState.RoundId > 1 && !OutsideDeviation(latestAnswer, *polledAnswer, thresholds) {
		l.Debugw("deviation < threshold, not submitting")

		return
	}

	if roundState.RoundId > 1 {
		l.Infow("deviation > threshold, starting new round")
	} else {
		l.Infow("starting first round")
	}

	// --> Create an ETH transaction by calling a 'submit' method on the contract

	// var payment assets.Link
	// if roundState.PaymentAmount == nil {
	// 	l.Error("roundState.PaymentAmount shouldn't be nil")
	// } else {
	// 	payment = assets.Link(*roundState.PaymentAmount)
	// }

	// err = p.createJobRun(polledAnswer, roundState.RoundId, &payment)
	// if err != nil {
	// 	l.Errorw("can't create job run", "err", err)
	// 	return
	// }

	promSetDecimal(promFMReportedValue.WithLabelValues(jobID), *polledAnswer)
	promSetUint32(promFMReportedRound.WithLabelValues(jobID), roundState.RoundId)
}

// If the polledAnswer is outside the allowable range, log an error and don't submit.
// to avoid an onchain reversion.
func (p *FluxMonitor) isValidSubmission(l *zap.SugaredLogger, polledAnswer decimal.Decimal) bool {
	max := decimal.NewFromBigInt(p.maxSubmission, -p.spec.Precision)
	min := decimal.NewFromBigInt(p.minSubmission, -p.spec.Precision)

	if polledAnswer.GreaterThan(max) || polledAnswer.LessThan(min) {
		l.Errorw("polled value is outside acceptable range", "min", min, "max", max, "polled value", polledAnswer)
		p.db.RecordError(p.spec.JobID, "Polled value is outside acceptable range")
		return false
	}
	return true
}

func (p *FluxMonitor) roundState(roundID uint32) (flux_aggregator_wrapper.OracleRoundState, error) {
	roundState, err := p.fluxAggregator.OracleRoundState(nil, p.oracleAddress, roundID)
	if err != nil {
		return flux_aggregator_wrapper.OracleRoundState{}, err
	}
	return roundState, nil
}

// initialRoundState fetches the round information that the fluxmonitor should use when starting
// new jobs. Choosing the correct round on startup is key to setting timers correctly.
func (p *FluxMonitor) initialRoundState() flux_aggregator_wrapper.OracleRoundState {
	defaultRoundState := flux_aggregator_wrapper.OracleRoundState{
		StartedAt: uint64(time.Now().Unix()),
	}
	latestRoundData, err := p.fluxAggregator.LatestRoundData(nil)
	if err != nil {
		logger.Warnf(
			"unable to retrieve latestRoundData for FluxAggregator contract %s - defaulting "+
				"to current time for tickers: %v",
			p.spec.ContractAddress.Hex(),
			err,
		)
		return defaultRoundState
	}
	roundID := uint32(latestRoundData.RoundId.Uint64())
	latestRoundState, err := p.fluxAggregator.OracleRoundState(nil, p.oracleAddress, roundID)
	if err != nil {
		logger.Warnf(
			"unable to call roundState for latest round, contract: %s, round: %d, err: %v",
			p.spec.ContractAddress.Hex(),
			latestRoundData.RoundId,
			err,
		)
		return defaultRoundState
	}
	return latestRoundState
}

func (p *FluxMonitor) resetTickers(roundState flux_aggregator_wrapper.OracleRoundState) {
	p.resetPollTicker()
	p.resetHibernationTimer()
	p.resetIdleTimer(roundState.StartedAt)
	p.resetRoundTimer(roundStateTimesOutAt(roundState))
}

func (p *FluxMonitor) setInitialTickers() {
	p.resetTickers(p.initialRoundState())
}

func (p *FluxMonitor) resetPollTicker() {
	if !p.spec.PollTimerDisabled && !p.isHibernating {
		p.pollTicker.Resume()
	} else {
		p.pollTicker.Pause()
	}
}

func (p *FluxMonitor) resetHibernationTimer() {
	if !p.isHibernating {
		p.hibernationTimer.Stop()
	} else {
		p.hibernationTimer.Reset(hibernationPollPeriod)
	}
}

func (p *FluxMonitor) resetRoundTimer(roundTimesOutAt uint64) {
	if p.isHibernating {
		p.roundTimer.Stop()
		return
	}

	loggerFields := p.loggerFields("timesOutAt", roundTimesOutAt)

	if roundTimesOutAt == 0 {
		p.roundTimer.Stop()
		logger.Debugw("disabling roundTimer, no active round", loggerFields...)

	} else {
		timesOutAt := time.Unix(int64(roundTimesOutAt), 0)
		timeUntilTimeout := time.Until(timesOutAt)

		if timeUntilTimeout <= 0 {
			p.roundTimer.Stop()
			logger.Debugw("roundTimer has run down; disabling", loggerFields...)
		} else {
			p.roundTimer.Reset(timeUntilTimeout)
			loggerFields = append(loggerFields, "value", roundTimesOutAt)
			logger.Debugw("updating roundState.TimesOutAt", loggerFields...)
		}
	}
}

func (p *FluxMonitor) resetIdleTimer(roundStartedAtUTC uint64) {
	if p.isHibernating || p.spec.IdleTimerDisabled {
		p.idleTimer.Stop()
		return
	} else if roundStartedAtUTC == 0 {
		// There is no active round, so keep using the idleTimer we already have
		return
	}

	startedAt := time.Unix(int64(roundStartedAtUTC), 0)
	idleDeadline := startedAt.Add(p.spec.IdleTimerPeriod)
	timeUntilIdleDeadline := time.Until(idleDeadline)
	loggerFields := p.loggerFields(
		"startedAt", roundStartedAtUTC,
		"timeUntilIdleDeadline", timeUntilIdleDeadline,
	)

	if timeUntilIdleDeadline <= 0 {
		logger.Debugw("not resetting idleTimer, negative duration", loggerFields...)
		return
	}
	p.idleTimer.Reset(timeUntilIdleDeadline)
	logger.Debugw("resetting idleTimer", loggerFields...)
}

// jobRunRequest is the request used to trigger a Job Run by the Flux Monitor.
type jobRunRequest struct {
	Result           decimal.Decimal `json:"result"`
	Address          string          `json:"address"`
	FunctionSelector string          `json:"functionSelector"`
	DataPrefix       string          `json:"dataPrefix"`
}

// func (p *PollingDeviationChecker) createJobRun(
// 	polledAnswer decimal.Decimal,
// 	roundID uint32,
// 	paymentAmount *assets.Link,
// ) error {

// 	methodID := fluxAggregatorABI.Methods["submit"].ID
// 	roundIDData := utils.EVMWordUint64(uint64(roundID))

// 	payload, err := json.Marshal(jobRunRequest{
// 		Result:           polledAnswer,
// 		Address:          p.spec.ContractAddress.Hex(),
// 		FunctionSelector: hexutil.Encode(methodID),
// 		DataPrefix:       hexutil.Encode(roundIDData),
// 	})
// 	if err != nil {
// 		return errors.Wrapf(err, "unable to encode Job Run request in JSON")
// 	}
// 	runData, err := models.ParseJSON(payload)
// 	if err != nil {
// 		return errors.Wrap(err, fmt.Sprintf("unable to start chainlink run with payload %s", payload))
// 	}
// 	runRequest := models.NewRunRequest(runData)
// 	runRequest.Payment = paymentAmount

// 	jobRun, err := p.runManager.Create(p.initr.JobSpecID, &p.initr, nil, runRequest)
// 	if err != nil {
// 		return err
// 	}

// 	err = p.store.UpdateFluxMonitorRoundStats(p.initr.Address, roundID, jobRun.ID)
// 	if err != nil {
// 		logger.Errorw(fmt.Sprintf("error updating FM round submission count: %v", err),
// 			"address", p.initr.Address.Hex(),
// 			"roundID", roundID,
// 			"jobID", p.initr.JobSpecID.String(),
// 		)
// 		return err
// 	}

// 	return nil
// }

func (p *FluxMonitor) loggerFields(added ...interface{}) []interface{} {
	return append(added, []interface{}{
		"pollFrequency", p.spec.PollTimerPeriod,
		"idleDuration", p.spec.IdleTimerPeriod,
		"contract", p.spec.ContractAddress.Hex(),
		"jobID", p.spec.JobID,
	}...)
}

func (p *FluxMonitor) loggerFieldsForNewRound(log flux_aggregator_wrapper.FluxAggregatorNewRound) []interface{} {
	return []interface{}{
		"round", log.RoundId,
		"startedBy", log.StartedBy.Hex(),
		"startedAt", log.StartedAt.String(),
		"contract", p.fluxAggregator.Address().Hex(),
		"jobID", p.spec.JobID,
	}
}

func (p *FluxMonitor) loggerFieldsForAnswerUpdated(log flux_aggregator_wrapper.FluxAggregatorAnswerUpdated) []interface{} {
	return []interface{}{
		"round", log.RoundId,
		"answer", log.Current.String(),
		"timestamp", log.UpdatedAt.String(),
		"contract", p.fluxAggregator.Address().Hex(),
		"job", p.spec.JobID,
	}
}

// OutsideDeviation checks whether the next price is outside the threshold.
// If both thresholds are zero (default value), always returns true.
func OutsideDeviation(curAnswer, nextAnswer decimal.Decimal, thresholds DeviationThresholds) bool {
	loggerFields := []interface{}{
		"threshold", thresholds.Rel,
		"absoluteThreshold", thresholds.Abs,
		"currentAnswer", curAnswer,
		"nextAnswer", nextAnswer,
	}

	if thresholds.Rel == 0 && thresholds.Abs == 0 {
		logger.Debugw(
			"Deviation thresholds both zero; short-circuiting deviation checker to "+
				"true, regardless of feed values", loggerFields...)
		return true
	}
	diff := curAnswer.Sub(nextAnswer).Abs()
	loggerFields = append(loggerFields, "absoluteDeviation", diff)

	if !diff.GreaterThan(decimal.NewFromFloat(thresholds.Abs)) {
		logger.Debugw("Absolute deviation threshold not met", loggerFields...)
		return false
	}

	if curAnswer.IsZero() {
		if nextAnswer.IsZero() {
			logger.Debugw("Relative deviation is undefined; can't satisfy threshold", loggerFields...)
			return false
		}
		logger.Infow("Threshold met: relative deviation is ∞", loggerFields...)
		return true
	}

	// 100*|new-old|/|old|: Deviation (relative to curAnswer) as a percentage
	percentage := diff.Div(curAnswer.Abs()).Mul(decimal.NewFromInt(100))

	loggerFields = append(loggerFields, "percentage", percentage)

	if percentage.LessThan(decimal.NewFromFloat(thresholds.Rel)) {
		logger.Debugw("Relative deviation threshold not met", loggerFields...)
		return false
	}
	logger.Infow("Relative and absolute deviation thresholds both met", loggerFields...)
	return true
}

func (p *FluxMonitor) statsAndStatusForRound(roundID uint32) (
	models.FluxMonitorRoundStats,
	models.RunStatus,
	error,
) {
	roundStats, err := p.db.FindOrCreateFluxMonitorRoundStats(p.spec.ContractAddress.Address(), roundID)
	if err != nil {
		return models.FluxMonitorRoundStats{}, "", err
	}
	// JobRun will not exist if this is the first time responding to this round
	var jobRun models.JobRun
	if roundStats.JobRunID.Valid {
		// INVESTIGATE -  what do we do here?
		// jobRun, err = p.store.FindJobRun(roundStats.JobRunID.UUID)
		// if err != nil {
		// 	return models.FluxMonitorRoundStats{}, "", err
		// }
	}
	return roundStats, jobRun.Status, nil
}

func roundStateTimesOutAt(rs flux_aggregator_wrapper.OracleRoundState) uint64 {
	return rs.StartedAt + rs.Timeout
}
