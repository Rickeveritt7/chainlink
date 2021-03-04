package fluxmonitorv2_test

import (
	"context"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/onsi/gomega"
	"github.com/pkg/errors"
	"github.com/shopspring/decimal"
	"github.com/smartcontractkit/chainlink/core/assets"
	"github.com/smartcontractkit/chainlink/core/internal/cltest"
	"github.com/smartcontractkit/chainlink/core/internal/gethwrappers/generated/flux_aggregator_wrapper"
	"github.com/smartcontractkit/chainlink/core/internal/mocks"
	"github.com/smartcontractkit/chainlink/core/logger"
	corenull "github.com/smartcontractkit/chainlink/core/null"
	"github.com/smartcontractkit/chainlink/core/services/fluxmonitorv2"
	fmmocks "github.com/smartcontractkit/chainlink/core/services/fluxmonitorv2/mocks"
	jobmocks "github.com/smartcontractkit/chainlink/core/services/job/mocks"
	logmocks "github.com/smartcontractkit/chainlink/core/services/log/mocks"
	"github.com/smartcontractkit/chainlink/core/services/pipeline"
	pipelinemocks "github.com/smartcontractkit/chainlink/core/services/pipeline/mocks"
	"github.com/smartcontractkit/chainlink/core/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"gopkg.in/guregu/null.v4"
)

const oracleCount uint8 = 17

type answerSet struct{ latestAnswer, polledAnswer int64 }

var (
	now     = func() uint64 { return uint64(time.Now().UTC().Unix()) }
	nilOpts *bind.CallOpts

	makeRoundDataForRoundID = func(roundID uint32) flux_aggregator_wrapper.LatestRoundData {
		return flux_aggregator_wrapper.LatestRoundData{
			RoundId: big.NewInt(int64(roundID)),
		}
	}
	freshContractRoundDataResponse = func() (flux_aggregator_wrapper.LatestRoundData, error) {
		return flux_aggregator_wrapper.LatestRoundData{}, errors.New("unstarted")
	}

	config = &fluxmonitorv2.Config{
		EthGasLimit: 21000,
	}
)

func NewSpecification() fluxmonitorv2.Specification {
	return fluxmonitorv2.Specification{
		ID:                "1",
		JobID:             1,
		ContractAddress:   cltest.NewAddress(),
		Precision:         2,
		Threshold:         0.5,
		AbsoluteThreshold: 0.01,
		PollTimerPeriod:   time.Minute,
		PollTimerDisabled: false,
		IdleTimerPeriod:   time.Minute,
		IdleTimerDisabled: false,
	}
}

func NewPipelineSpec() pipeline.Spec {
	return pipeline.Spec{
		ID: 1,
		DotDagSource: `
// data source 1
ds1 [type=http method=GET url="https://pricesource1.com" requestData="{\\"coin\\": \\"ETH\\", \\"market\\": \\"USD\\"}"];
ds1_parse [type=jsonparse path="latest"];

// data source 2
ds2 [type=http method=GET url="https://pricesource1.com" requestData="{\\"coin\\": \\"ETH\\", \\"market\\": \\"USD\\"}"];
ds2_parse [type=jsonparse path="latest"];

ds1 -> ds1_parse -> answer1;
ds2 -> ds2_parse -> answer1;

answer1 [type=median index=0];					
`,
	}
}

func NewPipelineRun() fluxmonitorv2.PipelineRun {
	jobID := int32(1)
	pipelineRunner := new(pipelinemocks.Runner)
	l := *logger.Default

	return fluxmonitorv2.NewPipelineRun(
		pipelineRunner,
		NewPipelineSpec(),
		jobID,
		l,
	)
}

func TestFluxMonitor_PollIfEligible(t *testing.T) {
	testCases := []struct {
		name              string
		eligible          bool
		connected         bool
		funded            bool
		answersDeviate    bool
		hasPreviousRun    bool
		previousRunStatus pipeline.RunStatus
		expectedToPoll    bool
		expectedToSubmit  bool
	}{
		{
			name:     "eligible",
			eligible: true, connected: true, funded: true, answersDeviate: true,
			expectedToPoll: true, expectedToSubmit: true,
		}, {
			name:     "ineligible",
			eligible: false, connected: true, funded: true, answersDeviate: true,
			expectedToPoll: false, expectedToSubmit: false,
		}, {
			name:     "disconnected",
			eligible: true, connected: false, funded: true, answersDeviate: true,
			expectedToPoll: false, expectedToSubmit: false,
		}, {
			name:     "under funded",
			eligible: true, connected: true, funded: false, answersDeviate: true,
			expectedToPoll: false, expectedToSubmit: false,
		}, {
			name:     "answer undeviated",
			eligible: true, connected: true, funded: true, answersDeviate: false,
			expectedToPoll: true, expectedToSubmit: false,
		}, {
			name:     "previous job run completed",
			eligible: true, connected: true, funded: true, answersDeviate: true,
			hasPreviousRun: true, previousRunStatus: pipeline.RunStatusCompleted,
			expectedToPoll: false, expectedToSubmit: false,
		}, {
			name:     "previous job run in progress",
			eligible: true, connected: true, funded: true, answersDeviate: true,
			hasPreviousRun: true, previousRunStatus: pipeline.RunStatusInProgress,
			expectedToPoll: false, expectedToSubmit: false,
			// There doesn't seem to be a way to check for cancelled
			// }, {
			// 	name:     "previous job run cancelled",
			// 	eligible: true, connected: true, funded: true, answersDeviate: true,
			// 	hasPreviousRun: true, previousRunStatus: models.RunStatusCancelled,
			// 	expectedToPoll: false, expectedToSubmit: false,
		}, {
			name:     "previous job run errored",
			eligible: true, connected: true, funded: true, answersDeviate: true,
			hasPreviousRun: true, previousRunStatus: pipeline.RunStatusErrored,
			expectedToPoll: true, expectedToSubmit: true,
		},
	}

	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)

	const reportableRoundID = 2

	var (
		thresholds        = struct{ abs, rel float64 }{0.1, 200}
		deviatedAnswers   = answerSet{1, 100}
		undeviatedAnswers = answerSet{100, 101}
	)

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				fluxAggregator    = new(mocks.FluxAggregator)
				logBroadcaster    = new(logmocks.Broadcaster)
				orm               = new(fmmocks.ORM)
				jobORM            = new(jobmocks.ORM)
				pipelineORM       = new(pipelinemocks.ORM)
				keyStore          = new(fmmocks.KeyStoreInterface)
				contractSubmitter = new(fmmocks.ContractSubmitter)
				spec              = fluxmonitorv2.Specification{
					ID:                "1",
					JobID:             1,
					ContractAddress:   cltest.NewAddress(),
					Precision:         2,
					Threshold:         0.5,
					AbsoluteThreshold: 0.01,
					PollTimerPeriod:   time.Minute,
					PollTimerDisabled: false,
					IdleTimerPeriod:   time.Minute,
					IdleTimerDisabled: false,
				}
			)
			t.Cleanup(func() {
				fluxAggregator.AssertExpectations(t)
				orm.AssertExpectations(t)
				keyStore.AssertExpectations(t)
				contractSubmitter.AssertExpectations(t)
			})

			keyStore.On("Accounts").Return([]accounts.Account{{Address: nodeAddr}}).Once()

			// Setup Answers
			answers := undeviatedAnswers
			if tc.answersDeviate {
				answers = deviatedAnswers
			}
			latestAnswerNoPrecision := answers.latestAnswer * int64(
				math.Pow10(int(spec.Precision)),
			)

			// Setup Run
			run := pipeline.Run{
				ID:             1,
				PipelineSpecID: 1,
			}
			if tc.hasPreviousRun {
				switch tc.previousRunStatus {
				case pipeline.RunStatusCompleted:
					now := time.Now()
					run.FinishedAt = &now
				case pipeline.RunStatusErrored:
					run.Errors = pipeline.JSONSerializable{
						Val:  pipeline.FinalErrors{null.StringFrom("Random: String, foo")},
						Null: false,
					}
				default:
				}

				orm.
					On("FindOrCreateFluxMonitorRoundStats", spec.ContractAddress, uint32(reportableRoundID)).
					Return(fluxmonitorv2.FluxMonitorRoundStatsV2{
						Aggregator:     spec.ContractAddress,
						RoundID:        reportableRoundID,
						PipelineRunID:  corenull.Int64From(run.ID),
						NumSubmissions: 1,
					}, nil)

				pipelineORM.
					On("FindRun", run.ID).
					Return(run, nil)
			} else {
				if tc.connected {
					orm.
						On("FindOrCreateFluxMonitorRoundStats", spec.ContractAddress, uint32(reportableRoundID)).
						Return(fluxmonitorv2.FluxMonitorRoundStatsV2{
							Aggregator: spec.ContractAddress,
							RoundID:    reportableRoundID,
						}, nil)
				}
			}

			// Set up funds
			var availableFunds *big.Int
			var paymentAmount *big.Int
			minPayment := store.Config.MinimumContractPayment().ToInt()
			if tc.funded {
				availableFunds = big.NewInt(1).Mul(big.NewInt(10000), minPayment)
				paymentAmount = minPayment
			} else {
				availableFunds = big.NewInt(1)
				paymentAmount = minPayment
			}

			roundState := flux_aggregator_wrapper.OracleRoundState{
				RoundId:          reportableRoundID,
				EligibleToSubmit: tc.eligible,
				LatestSubmission: big.NewInt(latestAnswerNoPrecision),
				AvailableFunds:   availableFunds,
				PaymentAmount:    paymentAmount,
				OracleCount:      oracleCount,
			}
			fluxAggregator.
				On("OracleRoundState", nilOpts, nodeAddr, uint32(0)).
				Return(roundState, nil).Maybe()

			// Setup the Pipeline Run
			pipelineRunner := new(pipelinemocks.Runner)
			pipelineSpec := NewPipelineSpec()
			l := *logger.Default

			pipelineRun := fluxmonitorv2.NewPipelineRun(
				pipelineRunner,
				pipelineSpec,
				spec.JobID,
				l,
			)

			if tc.expectedToPoll {
				pipelineRunner.
					On("ExecuteAndInsertNewRun", context.Background(), pipelineSpec, l).
					Return(int64(1), pipeline.FinalResult{
						Values: []interface{}{decimal.NewFromInt(answers.polledAnswer)},
						Errors: []error{nil},
					}, nil)
			}

			if tc.expectedToSubmit {
				contractSubmitter.
					On("Submit", big.NewInt(reportableRoundID), big.NewInt(answers.polledAnswer)).
					Return(nil).
					Once()

				orm.
					On("UpdateFluxMonitorRoundStats",
						spec.ContractAddress,
						uint32(reportableRoundID),
						int64(1),
					).
					Return(nil)
			}

			fm, err := fluxmonitorv2.NewFluxMonitor(
				pipelineRun,
				orm,
				jobORM,
				pipelineORM,
				keyStore,
				fluxmonitorv2.NewPollTicker(time.Minute, false),
				fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
				spec.ContractAddress,
				contractSubmitter,
				fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
				fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
				fluxmonitorv2.Flags{},
				fluxAggregator,
				logBroadcaster,
				spec,
				func() {},
			)
			require.NoError(t, err)

			if tc.connected {
				fm.OnConnect()
			}

			oracles := []common.Address{nodeAddr, cltest.NewAddress()}
			fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)
			fm.SetOracleAddress()

			fm.ExportedPollIfEligible(thresholds.rel, thresholds.abs)
		})
	}
}

// If the roundState method is unable to communicate with the contract (possibly due to
// incorrect address) then the pollIfEligible method should create a JobErr record
func TestFluxMonitor_PollIfEligible_Creates_JobErr(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()
	_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
	oracles := []common.Address{nodeAddr, cltest.NewAddress()}

	var (
		fluxAggregator    = new(mocks.FluxAggregator)
		logBroadcaster    = new(logmocks.Broadcaster)
		pipelineRunner    = new(pipelinemocks.Runner)
		orm               = new(fmmocks.ORM)
		jobORM            = new(jobmocks.ORM)
		pipelineORM       = new(pipelinemocks.ORM)
		keyStore          = new(fmmocks.KeyStoreInterface)
		contractSubmitter = new(fmmocks.ContractSubmitter)
		spec              = NewSpecification()
		pipelineSpec      = NewPipelineSpec()
		l                 = *logger.Default
		pipelineRun       = fluxmonitorv2.NewPipelineRun(
			pipelineRunner,
			pipelineSpec,
			spec.JobID,
			l,
		)
		roundState = flux_aggregator_wrapper.OracleRoundState{}
	)

	keyStore.On("Accounts").Return([]accounts.Account{{Address: nodeAddr}}).Once()

	jobORM.
		On("RecordError",
			context.Background(),
			spec.JobID,
			"Unable to call roundState method on provided contract. Check contract address.",
		).Once()

	fluxAggregator.
		On("OracleRoundState", nilOpts, nodeAddr, mock.Anything).
		Return(roundState, errors.New("err")).
		Once()

	fm, err := fluxmonitorv2.NewFluxMonitor(
		pipelineRun,
		orm,
		jobORM,
		pipelineORM,
		keyStore,
		fluxmonitorv2.NewPollTicker(time.Minute, false),
		fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
		spec.ContractAddress,
		contractSubmitter,
		fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
		fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
		fluxmonitorv2.Flags{},
		fluxAggregator,
		logBroadcaster,
		spec,
		func() {},
	)
	require.NoError(t, err)

	fm.OnConnect()

	fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)
	require.NoError(t, fm.SetOracleAddress())

	fm.ExportedPollIfEligible(1, 1)

	fluxAggregator.AssertExpectations(t)
	orm.AssertExpectations(t)
	keyStore.AssertExpectations(t)
}

func TestPollingDeviationChecker_BuffersLogs(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
	oracles := []common.Address{nodeAddr, cltest.NewAddress()}

	const (
		fetchedValue = 100
	)

	var (
		orm               = new(fmmocks.ORM)
		jobORM            = new(jobmocks.ORM)
		pipelineORM       = new(pipelinemocks.ORM)
		keyStore          = new(fmmocks.KeyStoreInterface)
		contractSubmitter = new(fmmocks.ContractSubmitter)
		spec              = NewSpecification()
	)
	spec.PollTimerDisabled = true
	spec.IdleTimerDisabled = true

	keyStore.On("Accounts").Return([]accounts.Account{{Address: nodeAddr}}).Once()

	// Test helpers
	var (
		makeRoundStateForRoundID = func(roundID uint32) flux_aggregator_wrapper.OracleRoundState {
			return flux_aggregator_wrapper.OracleRoundState{
				RoundId:          roundID,
				EligibleToSubmit: true,
				LatestSubmission: big.NewInt(100 * int64(math.Pow10(int(spec.Precision)))),
				AvailableFunds:   store.Config.MinimumContractPayment().ToInt(),
				PaymentAmount:    store.Config.MinimumContractPayment().ToInt(),
			}
		}
	)

	chBlock := make(chan struct{})
	chSafeToAssert := make(chan struct{})
	chSafeToFillQueue := make(chan struct{})

	pipelineRunner := new(pipelinemocks.Runner)
	pipelineSpec := NewPipelineSpec()
	l := *logger.Default

	pipelineRun := fluxmonitorv2.NewPipelineRun(
		pipelineRunner,
		pipelineSpec,
		spec.JobID,
		l,
	)

	fluxAggregator := new(mocks.FluxAggregator)
	fluxAggregator.On("LatestRoundData", nilOpts).Return(freshContractRoundDataResponse()).Once()
	fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(1)).
		Return(makeRoundStateForRoundID(1), nil).
		Run(func(mock.Arguments) {
			close(chSafeToFillQueue)
			<-chBlock
		}).
		Once()
	fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(3)).Return(makeRoundStateForRoundID(3), nil).Once()
	fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(4)).Return(makeRoundStateForRoundID(4), nil).Once()
	fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)
	fluxAggregator.On("Address").Return(spec.ContractAddress, nil)

	logBroadcaster := new(logmocks.Broadcaster)
	logBroadcaster.On("Register", mock.Anything, mock.Anything).Return(true)
	logBroadcaster.On("Unregister", mock.Anything, mock.Anything)

	orm.On("MostRecentFluxMonitorRoundID", spec.ContractAddress).Return(uint32(1), nil)
	orm.On("MostRecentFluxMonitorRoundID", spec.ContractAddress).Return(uint32(3), nil)
	orm.On("MostRecentFluxMonitorRoundID", spec.ContractAddress).Return(uint32(4), nil)

	// Round 1
	orm.
		On("FindOrCreateFluxMonitorRoundStats", spec.ContractAddress, uint32(1)).
		Return(fluxmonitorv2.FluxMonitorRoundStatsV2{
			Aggregator: spec.ContractAddress,
			RoundID:    1,
		}, nil)
	pipelineRunner.
		On("ExecuteAndInsertNewRun", context.Background(), pipelineSpec, l).
		Return(int64(1), pipeline.FinalResult{
			Values: []interface{}{decimal.NewFromInt(fetchedValue)},
			Errors: []error{nil},
		}, nil)
	contractSubmitter.
		On("Submit", big.NewInt(1), big.NewInt(fetchedValue)).
		Return(nil).
		Once()

	orm.
		On("UpdateFluxMonitorRoundStats",
			spec.ContractAddress,
			uint32(1),
			mock.AnythingOfType("int64"), //int64(1),
		).
		Return(nil).Once()

	// Round 3
	orm.
		On("FindOrCreateFluxMonitorRoundStats", spec.ContractAddress, uint32(3)).
		Return(fluxmonitorv2.FluxMonitorRoundStatsV2{
			Aggregator: spec.ContractAddress,
			RoundID:    3,
		}, nil)
	pipelineRunner.
		On("ExecuteAndInsertNewRun", context.Background(), pipelineSpec, l).
		Return(int64(2), pipeline.FinalResult{
			Values: []interface{}{decimal.NewFromInt(fetchedValue)},
			Errors: []error{nil},
		}, nil)
	contractSubmitter.
		On("Submit", big.NewInt(3), big.NewInt(fetchedValue)).
		Return(nil).
		Once()
	orm.
		On("UpdateFluxMonitorRoundStats",
			spec.ContractAddress,
			uint32(3),
			mock.AnythingOfType("int64"), //int64(2),

		).
		Return(nil).Once()

	// Round 4
	orm.
		On("FindOrCreateFluxMonitorRoundStats", spec.ContractAddress, uint32(4)).
		Return(fluxmonitorv2.FluxMonitorRoundStatsV2{
			Aggregator: spec.ContractAddress,
			RoundID:    3,
		}, nil)
	pipelineRunner.
		On("ExecuteAndInsertNewRun", context.Background(), pipelineSpec, l).
		Return(int64(3), pipeline.FinalResult{
			Values: []interface{}{decimal.NewFromInt(fetchedValue)},
			Errors: []error{nil},
		}, nil)
	contractSubmitter.
		On("Submit", big.NewInt(4), big.NewInt(fetchedValue)).
		Return(nil).
		Once()
	orm.
		On("UpdateFluxMonitorRoundStats",
			spec.ContractAddress,
			uint32(4),
			mock.AnythingOfType("int64"), //int64(3),
		).
		Return(nil).
		Once().
		Run(func(mock.Arguments) { close(chSafeToAssert) })

	checker, err := fluxmonitorv2.NewFluxMonitor(
		pipelineRun,
		orm,
		jobORM,
		pipelineORM,
		keyStore,
		fluxmonitorv2.NewPollTicker(time.Minute, true),
		fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
		spec.ContractAddress,
		contractSubmitter,
		fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
		fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
		fluxmonitorv2.Flags{},
		fluxAggregator,
		logBroadcaster,
		spec,
		func() {},
	)
	require.NoError(t, err)

	checker.OnConnect()
	checker.Start()

	var logBroadcasts []*logmocks.Broadcast

	for i := 1; i <= 4; i++ {
		logBroadcast := new(logmocks.Broadcast)
		logBroadcast.On("DecodedLog").Return(&flux_aggregator_wrapper.FluxAggregatorNewRound{RoundId: big.NewInt(int64(i)), StartedAt: big.NewInt(0)})
		logBroadcast.On("WasAlreadyConsumed").Return(false, nil)
		logBroadcast.On("MarkConsumed").Return(nil)
		logBroadcasts = append(logBroadcasts, logBroadcast)
	}

	checker.HandleLog(logBroadcasts[0], nil) // Get the checker to start processing a log so we can freeze it
	<-chSafeToFillQueue
	checker.HandleLog(logBroadcasts[1], nil) // This log is evicted from the priority queue
	checker.HandleLog(logBroadcasts[2], nil)
	checker.HandleLog(logBroadcasts[3], nil)

	close(chBlock)
	<-chSafeToAssert

	fluxAggregator.AssertExpectations(t)
	pipelineRunner.AssertExpectations(t)
	orm.AssertExpectations(t)
	keyStore.AssertExpectations(t)
	contractSubmitter.AssertExpectations(t)
}

func TestFluxMonitor_TriggerIdleTimeThreshold(t *testing.T) {
	testCases := []struct {
		name              string
		idleTimerDisabled bool
		idleDuration      time.Duration
		expectedToSubmit  bool
	}{
		{"no idleDuration", true, 0, false},
		{"idleDuration > 0", false, 2 * time.Second, true},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, cleanup := cltest.NewStore(t)
			defer cleanup()

			_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
			oracles := []common.Address{nodeAddr, cltest.NewAddress()}

			var (
				fluxAggregator = new(mocks.FluxAggregator)
				logBroadcast   = new(logmocks.Broadcast)
				logBroadcaster = new(logmocks.Broadcaster)
				jobORM         = new(jobmocks.ORM)
				pipelineORM    = new(pipelinemocks.ORM)
				keyStore       = new(fmmocks.KeyStoreInterface)
				spec           = NewSpecification()
				pollTicker     = fluxmonitorv2.NewPollTicker(time.Minute, true)
				orm            = fluxmonitorv2.NewORM(store.DB)
			)
			spec.PollTimerDisabled = true
			spec.IdleTimerDisabled = tc.idleTimerDisabled
			spec.IdleTimerPeriod = tc.idleDuration

			keyStore.On("Accounts").Return([]accounts.Account{{Address: nodeAddr}}).Once()

			const fetchedAnswer = 100
			answerBigInt := big.NewInt(fetchedAnswer * int64(math.Pow10(int(spec.Precision))))

			fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)
			fluxAggregator.On("Address").Return(spec.ContractAddress).Maybe()
			logBroadcaster.On("Register", mock.Anything, mock.Anything).Return(true)
			logBroadcaster.On("Unregister", mock.Anything, mock.Anything)

			idleDurationOccured := make(chan struct{}, 3)

			fluxAggregator.On("LatestRoundData", nilOpts).Return(freshContractRoundDataResponse()).Once()
			if tc.expectedToSubmit {
				// performInitialPoll()
				roundState1 := flux_aggregator_wrapper.OracleRoundState{RoundId: 1, EligibleToSubmit: false, LatestSubmission: answerBigInt, StartedAt: now()}
				fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(0)).Return(roundState1, nil).Once()
				// idleDuration 1
				roundState2 := flux_aggregator_wrapper.OracleRoundState{RoundId: 1, EligibleToSubmit: false, LatestSubmission: answerBigInt, StartedAt: now()}
				fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(0)).Return(roundState2, nil).Once().Run(func(args mock.Arguments) {
					idleDurationOccured <- struct{}{}
				})
			}

			fm, err := fluxmonitorv2.NewFluxMonitor(
				NewPipelineRun(),
				orm,
				jobORM,
				pipelineORM,
				keyStore,
				pollTicker,
				fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
				spec.ContractAddress,
				fluxmonitorv2.NewFluxAggregatorContractSubmitter(fluxAggregator, orm, keyStore, config.EthGasLimit),
				fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
				fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
				fluxmonitorv2.Flags{},
				fluxAggregator,
				logBroadcaster,
				spec,
				func() {},
			)
			require.NoError(t, err)

			fm.OnConnect()
			fm.Start()
			require.Len(t, idleDurationOccured, 0, "no Job Runs created")

			if tc.expectedToSubmit {
				require.Eventually(t, func() bool { return len(idleDurationOccured) == 1 }, 3*time.Second, 10*time.Millisecond)

				chBlock := make(chan struct{})
				// NewRound resets the idle timer
				roundState2 := flux_aggregator_wrapper.OracleRoundState{RoundId: 2, EligibleToSubmit: false, LatestSubmission: answerBigInt, StartedAt: now()}
				fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(2)).Return(roundState2, nil).Once().Run(func(args mock.Arguments) {
					close(chBlock)
				})

				decodedLog := flux_aggregator_wrapper.FluxAggregatorNewRound{RoundId: big.NewInt(2), StartedAt: big.NewInt(0)}
				logBroadcast.On("DecodedLog").Return(&decodedLog)
				logBroadcast.On("WasAlreadyConsumed").Return(false, nil).Once()
				logBroadcast.On("MarkConsumed").Return(nil).Once()
				fm.HandleLog(logBroadcast, nil)

				gomega.NewGomegaWithT(t).Eventually(chBlock).Should(gomega.BeClosed())

				// idleDuration 2
				roundState3 := flux_aggregator_wrapper.OracleRoundState{RoundId: 3, EligibleToSubmit: false, LatestSubmission: answerBigInt, StartedAt: now()}
				fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(0)).Return(roundState3, nil).Once().Run(func(args mock.Arguments) {
					idleDurationOccured <- struct{}{}
				})
				require.Eventually(t, func() bool { return len(idleDurationOccured) == 2 }, 3*time.Second, 10*time.Millisecond)
			}

			fm.Close()

			if !tc.expectedToSubmit {
				require.Len(t, idleDurationOccured, 0)
			}

			fluxAggregator.AssertExpectations(t)
			keyStore.AssertExpectations(t)
		})
	}
}

func TestFluxMonitor_RoundTimeoutCausesPoll_timesOutAtZero(t *testing.T) {
	t.Parallel()

	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
	oracles := []common.Address{nodeAddr, cltest.NewAddress()}

	var (
		fluxAggregator = new(mocks.FluxAggregator)
		logBroadcaster = new(logmocks.Broadcaster)
		orm            = fluxmonitorv2.NewORM(store.DB)
		jobORM         = new(jobmocks.ORM)
		pipelineORM    = new(pipelinemocks.ORM)
		keyStore       = new(fmmocks.KeyStoreInterface)
		spec           = NewSpecification()
		pollTicker     = fluxmonitorv2.NewPollTicker(time.Minute, true)
	)
	spec.PollTimerDisabled = true
	spec.IdleTimerDisabled = true

	keyStore.
		On("Accounts").
		Return([]accounts.Account{{Address: nodeAddr}}).
		Twice() // Once called from the test, once during start

	ch := make(chan struct{})

	const fetchedAnswer = 100
	answerBigInt := big.NewInt(fetchedAnswer * int64(math.Pow10(int(spec.Precision))))
	logBroadcaster.On("Register", mock.Anything, mock.Anything).Return(true)
	logBroadcaster.On("Unregister", mock.Anything, mock.Anything)

	fluxAggregator.On("LatestRoundData", nilOpts).Return(makeRoundDataForRoundID(1), nil).Once()
	fluxAggregator.On("Address").Return(spec.ContractAddress).Maybe()
	roundState0 := flux_aggregator_wrapper.OracleRoundState{RoundId: 1, EligibleToSubmit: false, LatestSubmission: answerBigInt, StartedAt: now()}
	fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(1)).Return(roundState0, nil).Once() // initialRoundState()
	fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(0)).Return(flux_aggregator_wrapper.OracleRoundState{
		RoundId:          1,
		EligibleToSubmit: false,
		LatestSubmission: answerBigInt,
		StartedAt:        0,
		Timeout:          0,
	}, nil).
		Run(func(mock.Arguments) { close(ch) }).
		Once()

	fm, err := fluxmonitorv2.NewFluxMonitor(
		NewPipelineRun(),
		orm,
		jobORM,
		pipelineORM,
		keyStore,
		pollTicker,
		fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
		spec.ContractAddress,
		fluxmonitorv2.NewFluxAggregatorContractSubmitter(fluxAggregator, orm, keyStore, config.EthGasLimit),
		fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
		fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
		fluxmonitorv2.Flags{},
		fluxAggregator,
		logBroadcaster,
		spec,
		func() {},
	)
	require.NoError(t, err)

	fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)

	fm.SetOracleAddress()
	fm.ExportedRoundState()
	fm.Start()
	fm.OnConnect()

	gomega.NewGomegaWithT(t).Eventually(ch).Should(gomega.BeClosed())

	fm.Close()

	fluxAggregator.AssertExpectations(t)
	keyStore.AssertExpectations(t)
}

func TestFluxMonitor_UsesPreviousRoundStateOnStartup_RoundTimeout(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
	oracles := []common.Address{nodeAddr, cltest.NewAddress()}

	var (
		logBroadcaster = new(logmocks.Broadcaster)
		spec           = NewSpecification()
	)

	spec.PollTimerDisabled = true
	spec.IdleTimerDisabled = true

	tests := []struct {
		name             string
		timeout          uint64
		expectedToSubmit bool
	}{
		{"active round exists - round will time out", 2, true},
		{"active round exists - round will not time out", 100, false},
		{"no active round", 0, false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var (
				pollTicker     = fluxmonitorv2.NewPollTicker(time.Minute, true)
				fluxAggregator = new(mocks.FluxAggregator)
				jobORM         = new(jobmocks.ORM)
				orm            = fluxmonitorv2.NewORM(store.DB)
				pipelineORM    = new(pipelinemocks.ORM)
				keyStore       = new(fmmocks.KeyStoreInterface)
			)

			keyStore.On("Accounts").Return([]accounts.Account{{Address: nodeAddr}}).Once()

			logBroadcaster.On("Register", mock.Anything, mock.Anything).Return(true)
			logBroadcaster.On("Unregister", mock.Anything, mock.Anything)

			fluxAggregator.On("Address").Return(spec.ContractAddress).Maybe()
			fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)

			fluxAggregator.On("LatestRoundData", nilOpts).Return(makeRoundDataForRoundID(1), nil).Once()
			fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(1)).Return(flux_aggregator_wrapper.OracleRoundState{
				RoundId:          1,
				EligibleToSubmit: false,
				StartedAt:        now(),
				Timeout:          test.timeout,
			}, nil).Once()

			// 2nd roundstate call means round timer triggered
			chRoundState := make(chan struct{})
			fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(0)).Return(flux_aggregator_wrapper.OracleRoundState{
				RoundId:          1,
				EligibleToSubmit: false,
			}, nil).
				Run(func(mock.Arguments) { close(chRoundState) }).
				Maybe()

			fm, err := fluxmonitorv2.NewFluxMonitor(
				NewPipelineRun(),
				orm,
				jobORM,
				pipelineORM,
				keyStore,
				pollTicker,
				fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
				spec.ContractAddress,
				fluxmonitorv2.NewFluxAggregatorContractSubmitter(fluxAggregator, orm, keyStore, config.EthGasLimit),
				fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
				fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
				fluxmonitorv2.Flags{},
				fluxAggregator,
				logBroadcaster,
				spec,
				func() {},
			)
			require.NoError(t, err)

			fm.Start()
			fm.OnConnect()

			if test.expectedToSubmit {
				gomega.NewGomegaWithT(t).Eventually(chRoundState).Should(gomega.BeClosed())
			} else {
				gomega.NewGomegaWithT(t).Consistently(chRoundState).ShouldNot(gomega.BeClosed())
			}

			fm.Close()
			fluxAggregator.AssertExpectations(t)
			keyStore.AssertExpectations(t)
		})
	}
}

func TestFluxMonitor_UsesPreviousRoundStateOnStartup_IdleTimer(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
	oracles := []common.Address{nodeAddr, cltest.NewAddress()}

	var (
		logBroadcaster = new(logmocks.Broadcaster)
		spec           = NewSpecification()
	)
	spec.PollTimerDisabled = true
	spec.IdleTimerDisabled = false

	almostExpired := time.Now().
		Add(spec.IdleTimerPeriod * -1).
		Add(2 * time.Second).
		Unix()

	testCases := []struct {
		name             string
		startedAt        uint64
		expectedToSubmit bool
	}{
		{"active round exists - idleTimer about to expired", uint64(almostExpired), true},
		{"active round exists - idleTimer will not expire", 100, false},
		{"no active round", 0, false},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				pollTicker     = fluxmonitorv2.NewPollTicker(time.Minute, true)
				fluxAggregator = new(mocks.FluxAggregator)
				orm            = fluxmonitorv2.NewORM(store.DB)
				jobORM         = new(jobmocks.ORM)
				pipelineORM    = new(pipelinemocks.ORM)
				keyStore       = new(fmmocks.KeyStoreInterface)
			)

			keyStore.On("Accounts").Return([]accounts.Account{{Address: nodeAddr}}).Once()

			logBroadcaster.On("Register", mock.Anything, mock.Anything).Return(true)
			logBroadcaster.On("Unregister", mock.Anything, mock.Anything)

			fluxAggregator.On("Address").Return(spec.ContractAddress).Maybe()
			fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)
			fluxAggregator.On("LatestRoundData", nilOpts).Return(makeRoundDataForRoundID(1), nil).Once()
			// first roundstate in setInitialTickers()
			fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(1)).Return(flux_aggregator_wrapper.OracleRoundState{
				RoundId:          1,
				EligibleToSubmit: false,
				StartedAt:        tc.startedAt,
				Timeout:          10000, // round won't time out
			}, nil).Once()

			// 2nd roundstate in performInitialPoll()
			roundState := flux_aggregator_wrapper.OracleRoundState{RoundId: 1, EligibleToSubmit: false}
			fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(0)).Return(roundState, nil).Once()

			// 3rd roundState call means idleTimer triggered
			chRoundState := make(chan struct{})
			fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(0)).Return(roundState, nil).
				Run(func(mock.Arguments) { close(chRoundState) }).
				Maybe()

			fm, err := fluxmonitorv2.NewFluxMonitor(
				NewPipelineRun(),
				orm,
				jobORM,
				pipelineORM,
				keyStore,
				pollTicker,
				fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
				spec.ContractAddress,
				fluxmonitorv2.NewFluxAggregatorContractSubmitter(fluxAggregator, orm, keyStore, config.EthGasLimit),
				fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
				fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
				fluxmonitorv2.Flags{},
				fluxAggregator,
				logBroadcaster,
				spec,
				func() {},
			)
			require.NoError(t, err)

			fm.Start()
			fm.OnConnect()

			if tc.expectedToSubmit {
				gomega.NewGomegaWithT(t).Eventually(chRoundState).Should(gomega.BeClosed())
			} else {
				gomega.NewGomegaWithT(t).Consistently(chRoundState).ShouldNot(gomega.BeClosed())
			}

			fm.Close()

			fluxAggregator.AssertExpectations(t)
			keyStore.AssertExpectations(t)
		})
	}
}

func TestFluxMonitor_RoundTimeoutCausesPoll_timesOutNotZero(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
	oracles := []common.Address{nodeAddr, cltest.NewAddress()}

	var (
		fluxAggregator = new(mocks.FluxAggregator)
		logBroadcaster = new(logmocks.Broadcaster)
		jobORM         = new(jobmocks.ORM)
		orm            = fluxmonitorv2.NewORM(store.DB)
		pipelineORM    = new(pipelinemocks.ORM)
		spec           = NewSpecification()
		keyStore       = new(fmmocks.KeyStoreInterface)
		pollTicker     = fluxmonitorv2.NewPollTicker(time.Minute, true)
	)
	spec.PollTimerDisabled = true
	spec.IdleTimerDisabled = true

	keyStore.On("Accounts").Return([]accounts.Account{{Address: nodeAddr}}).Once()

	const fetchedAnswer = 100
	answerBigInt := big.NewInt(fetchedAnswer * int64(math.Pow10(int(spec.Precision))))

	chRoundState1 := make(chan struct{})
	chRoundState2 := make(chan struct{})

	logBroadcaster.On("Register", mock.Anything, mock.Anything).Return(true)
	logBroadcaster.On("Unregister", mock.Anything, mock.Anything)

	fluxAggregator.On("Address").Return(spec.ContractAddress).Maybe()
	fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)
	fluxAggregator.On("LatestRoundData", nilOpts).Return(makeRoundDataForRoundID(1), nil).Once()
	fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(1)).Return(flux_aggregator_wrapper.OracleRoundState{
		RoundId:          1,
		EligibleToSubmit: false,
		LatestSubmission: answerBigInt,
		StartedAt:        now(),
		Timeout:          uint64(1000000),
	}, nil).Once()

	startedAt := uint64(time.Now().Unix())
	timeout := uint64(3)
	fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(0)).Return(flux_aggregator_wrapper.OracleRoundState{
		RoundId:          1,
		EligibleToSubmit: false,
		LatestSubmission: answerBigInt,
		StartedAt:        startedAt,
		Timeout:          timeout,
	}, nil).Once().
		Run(func(mock.Arguments) { close(chRoundState1) }).
		Once()
	fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(0)).Return(flux_aggregator_wrapper.OracleRoundState{
		RoundId:          1,
		EligibleToSubmit: false,
		LatestSubmission: answerBigInt,
		StartedAt:        startedAt,
		Timeout:          timeout,
	}, nil).Once().
		Run(func(mock.Arguments) { close(chRoundState2) }).
		Once()

	fm, err := fluxmonitorv2.NewFluxMonitor(
		NewPipelineRun(),
		orm,
		jobORM,
		pipelineORM,
		keyStore,
		pollTicker,
		fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
		spec.ContractAddress,
		fluxmonitorv2.NewFluxAggregatorContractSubmitter(fluxAggregator, orm, keyStore, config.EthGasLimit),
		fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
		fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
		fluxmonitorv2.Flags{},
		fluxAggregator,
		logBroadcaster,
		spec,
		func() {},
	)
	require.NoError(t, err)

	fm.Start()
	fm.OnConnect()

	logBroadcast := new(logmocks.Broadcast)
	logBroadcast.On("WasAlreadyConsumed").Return(false, nil)
	logBroadcast.On("DecodedLog").Return(&flux_aggregator_wrapper.FluxAggregatorNewRound{RoundId: big.NewInt(0), StartedAt: big.NewInt(time.Now().UTC().Unix())})
	logBroadcast.On("MarkConsumed").Return(nil)
	fm.HandleLog(logBroadcast, nil)

	gomega.NewGomegaWithT(t).Eventually(chRoundState1).Should(gomega.BeClosed())
	gomega.NewGomegaWithT(t).Eventually(chRoundState2).Should(gomega.BeClosed())

	time.Sleep(time.Duration(2*timeout) * time.Second)
	fm.Close()

	fluxAggregator.AssertExpectations(t)
	keyStore.AssertExpectations(t)
}

func TestFluxMonitor_IsFlagLowered(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		getFlagsResult []bool
		expected       bool
	}{
		{"both lowered", []bool{false, false}, true},
		{"global lowered", []bool{false, true}, true},
		{"contract lowered", []bool{true, false}, true},
		{"both raised", []bool{true, true}, false},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				fluxAggregator = new(mocks.FluxAggregator)
				logBroadcaster = new(logmocks.Broadcaster)
				flagsContract  = new(mocks.Flags)
				flags          = fluxmonitorv2.Flags{FlagsInterface: flagsContract}
				jobORM         = new(jobmocks.ORM)
				orm            = new(fmmocks.ORM)
				pipelineORM    = new(pipelinemocks.ORM)
				pollTicker     = fluxmonitorv2.NewPollTicker(time.Minute, false)
				spec           = NewSpecification()
				keyStore       = new(fmmocks.KeyStoreInterface)
			)

			flagsContract.On("GetFlags", mock.Anything, mock.Anything).
				Run(func(args mock.Arguments) {
					require.Equal(t, []common.Address{utils.ZeroAddress, spec.ContractAddress}, args.Get(1).([]common.Address))
				}).
				Return(tc.getFlagsResult, nil)

			fm, err := fluxmonitorv2.NewFluxMonitor(
				NewPipelineRun(),
				orm,
				jobORM,
				pipelineORM,
				keyStore,
				pollTicker,
				fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
				spec.ContractAddress,
				fluxmonitorv2.NewFluxAggregatorContractSubmitter(fluxAggregator, orm, keyStore, config.EthGasLimit),
				fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
				fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
				flags,
				fluxAggregator,
				logBroadcaster,
				spec,
				func() {},
			)
			require.NoError(t, err)

			result, err := fm.ExportedIsFlagLowered()
			require.NoError(t, err)
			require.Equal(t, tc.expected, result)

			flagsContract.AssertExpectations(t)
		})
	}
}

func TestFluxMonitor_HandlesNilLogs(t *testing.T) {
	t.Parallel()

	var (
		fluxAggregator = new(mocks.FluxAggregator)
		logBroadcaster = new(logmocks.Broadcaster)
		jobORM         = new(jobmocks.ORM)
		pipelineORM    = new(pipelinemocks.ORM)
		spec           = NewSpecification()
		pollTicker     = fluxmonitorv2.NewPollTicker(time.Minute, false)
		orm            = new(fmmocks.ORM)
		keyStore       = new(fmmocks.KeyStoreInterface)
	)

	fm, err := fluxmonitorv2.NewFluxMonitor(
		NewPipelineRun(),
		orm,
		jobORM,
		pipelineORM,
		keyStore,
		pollTicker,
		fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
		spec.ContractAddress,
		fluxmonitorv2.NewFluxAggregatorContractSubmitter(fluxAggregator, orm, keyStore, config.EthGasLimit),
		fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
		fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
		fluxmonitorv2.Flags{},
		fluxAggregator,
		logBroadcaster,
		spec,
		func() {},
	)
	require.NoError(t, err)

	logBroadcast := new(logmocks.Broadcast)
	var logNewRound *flux_aggregator_wrapper.FluxAggregatorNewRound
	var logAnswerUpdated *flux_aggregator_wrapper.FluxAggregatorAnswerUpdated
	var randomType interface{}

	logBroadcast.On("DecodedLog").Return(logNewRound).Once()
	assert.NotPanics(t, func() {
		fm.HandleLog(logBroadcast, nil)
	})

	logBroadcast.On("DecodedLog").Return(logAnswerUpdated).Once()
	assert.NotPanics(t, func() {
		fm.HandleLog(logBroadcast, nil)
	})

	logBroadcast.On("DecodedLog").Return(randomType).Once()
	assert.NotPanics(t, func() {
		fm.HandleLog(logBroadcast, nil)
	})

	logBroadcast.AssertExpectations(t)
}

func TestFluxMonitor_ConsumeLogBroadcast(t *testing.T) {
	t.Parallel()

	var (
		fluxAggregator = new(mocks.FluxAggregator)
		logBroadcaster = new(logmocks.Broadcaster)
		jobORM         = new(jobmocks.ORM)
		pipelineORM    = new(pipelinemocks.ORM)
		spec           = NewSpecification()
		pollTicker     = fluxmonitorv2.NewPollTicker(time.Minute, false)
		orm            = new(fmmocks.ORM)
		keyStore       = new(fmmocks.KeyStoreInterface)
	)

	fm, err := fluxmonitorv2.NewFluxMonitor(
		NewPipelineRun(),
		orm,
		jobORM,
		pipelineORM,
		keyStore,
		pollTicker,
		fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
		spec.ContractAddress,
		fluxmonitorv2.NewFluxAggregatorContractSubmitter(fluxAggregator, orm, keyStore, config.EthGasLimit),
		fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
		fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
		fluxmonitorv2.Flags{},
		fluxAggregator,
		logBroadcaster,
		spec,
		func() {},
	)
	require.NoError(t, err)

	fluxAggregator.
		On("OracleRoundState", nilOpts, mock.Anything, mock.Anything).
		Return(flux_aggregator_wrapper.OracleRoundState{RoundId: 123}, nil)
	fluxAggregator.
		On("Address").
		Return(cltest.NewAddress())

	logBroadcast := new(logmocks.Broadcast)
	logBroadcast.On("WasAlreadyConsumed").Return(false, nil).Once()
	logBroadcast.On("DecodedLog").Return(&flux_aggregator_wrapper.FluxAggregatorAnswerUpdated{})
	logBroadcast.On("MarkConsumed").Return(nil).Once()

	fm.ExportedBacklog().Add(fluxmonitorv2.PriorityNewRoundLog, logBroadcast)
	fm.ExportedProcessLogs()

	logBroadcast.AssertExpectations(t)
}

func TestFluxMonitor_ConsumeLogBroadcast_Error(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		consumed bool
		err      error
	}{
		{"already consumed", true, nil},
		{"error determining already consumed", false, errors.New("err")},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				fluxAggregator = new(mocks.FluxAggregator)
				logBroadcaster = new(logmocks.Broadcaster)
				orm            = new(fmmocks.ORM)
				jobORM         = new(jobmocks.ORM)
				pipelineORM    = new(pipelinemocks.ORM)
				keyStore       = new(fmmocks.KeyStoreInterface)
				spec           = NewSpecification()
				pollTicker     = fluxmonitorv2.NewPollTicker(time.Minute, false)
			)

			fm, err := fluxmonitorv2.NewFluxMonitor(
				NewPipelineRun(),
				orm,
				jobORM,
				pipelineORM,
				keyStore,
				pollTicker,
				fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
				spec.ContractAddress,
				fluxmonitorv2.NewFluxAggregatorContractSubmitter(fluxAggregator, orm, keyStore, config.EthGasLimit),
				fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
				fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
				fluxmonitorv2.Flags{},
				fluxAggregator,
				logBroadcaster,
				spec,
				func() {},
			)
			require.NoError(t, err)

			logBroadcast := new(logmocks.Broadcast)
			logBroadcast.On("WasAlreadyConsumed").Return(tc.consumed, tc.err).Once()

			fm.ExportedBacklog().Add(fluxmonitorv2.PriorityNewRoundLog, logBroadcast)
			fm.ExportedProcessLogs()

			logBroadcast.AssertExpectations(t)
		})
	}
}

func TestFluxMonitor_DoesNotDoubleSubmit(t *testing.T) {
	t.Run("when NewRound log arrives, then poll ticker fires", func(t *testing.T) {
		store, cleanup := cltest.NewStore(t)
		defer cleanup()

		_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
		oracles := []common.Address{nodeAddr, cltest.NewAddress()}

		var (
			fluxAggregator    = new(mocks.FluxAggregator)
			logBroadcaster    = new(logmocks.Broadcaster)
			orm               = new(fmmocks.ORM)
			jobORM            = new(jobmocks.ORM)
			pipelineORM       = new(pipelinemocks.ORM)
			keyStore          = new(fmmocks.KeyStoreInterface)
			contractSubmitter = new(fmmocks.ContractSubmitter)
			pollTicker        = fluxmonitorv2.NewPollTicker(time.Minute, true)

			spec           = NewSpecification()
			paymentAmount  = store.Config.MinimumContractPayment().ToInt()
			availableFunds = big.NewInt(1).Mul(paymentAmount, big.NewInt(1000))
		)
		spec.PollTimerDisabled = true
		spec.IdleTimerDisabled = true

		// Setup the Pipeline Run
		pipelineRunner := new(pipelinemocks.Runner)
		pipelineSpec := NewPipelineSpec()
		l := *logger.Default

		pipelineRun := fluxmonitorv2.NewPipelineRun(
			pipelineRunner,
			pipelineSpec,
			spec.JobID,
			l,
		)

		const (
			roundID = 3
			answer  = 100
		)

		keyStore.On("Accounts").Return([]accounts.Account{{Address: nodeAddr}}).Once()

		fm, err := fluxmonitorv2.NewFluxMonitor(
			pipelineRun,
			orm,
			jobORM,
			pipelineORM,
			keyStore,
			pollTicker,
			fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
			spec.ContractAddress,
			contractSubmitter,
			fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
			fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
			fluxmonitorv2.Flags{},
			fluxAggregator,
			logBroadcaster,
			spec,
			func() {},
		)
		require.NoError(t, err)

		// Mocks initiated by the New Round log
		orm.On("MostRecentFluxMonitorRoundID", spec.ContractAddress).Return(uint32(roundID), nil).Once()
		orm.
			On("FindOrCreateFluxMonitorRoundStats", spec.ContractAddress, uint32(roundID)).
			Return(fluxmonitorv2.FluxMonitorRoundStatsV2{
				Aggregator: spec.ContractAddress,
				RoundID:    roundID,
			}, nil).Once()
		pipelineRunner.
			On("ExecuteAndInsertNewRun", context.Background(), pipelineSpec, l).
			Return(int64(1), pipeline.FinalResult{
				Values: []interface{}{decimal.NewFromInt(answer)},
				Errors: []error{nil},
			}, nil).Once()
		contractSubmitter.On("Submit", big.NewInt(roundID), big.NewInt(answer)).Return(nil).Once()
		orm.
			On("UpdateFluxMonitorRoundStats",
				spec.ContractAddress,
				uint32(roundID),
				int64(1),
			).
			Return(nil)

		fluxAggregator.On("Address").Return(spec.ContractAddress)
		fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)
		fm.SetOracleAddress()
		fm.OnConnect()

		// Fire off the NewRound log, which the node should respond to
		fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(roundID)).
			Return(flux_aggregator_wrapper.OracleRoundState{
				RoundId:          roundID,
				LatestSubmission: big.NewInt(answer),
				EligibleToSubmit: true,
				AvailableFunds:   availableFunds,
				PaymentAmount:    paymentAmount,
				OracleCount:      1,
			}, nil).
			Once()

		fm.ExportedRespondToNewRoundLog(&flux_aggregator_wrapper.FluxAggregatorNewRound{
			RoundId:   big.NewInt(roundID),
			StartedAt: big.NewInt(0),
		})

		// Mocks initiated by polling
		// Now force the node to try to poll and ensure it does not respond this time
		fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(0)).
			Return(flux_aggregator_wrapper.OracleRoundState{
				RoundId:          roundID,
				LatestSubmission: big.NewInt(answer),
				EligibleToSubmit: true,
				AvailableFunds:   availableFunds,
				PaymentAmount:    paymentAmount,
				OracleCount:      1,
			}, nil).
			Once()
		orm.
			On("FindOrCreateFluxMonitorRoundStats", spec.ContractAddress, uint32(roundID)).
			Return(fluxmonitorv2.FluxMonitorRoundStatsV2{
				PipelineRunID:  corenull.NewInt64(int64(1), true),
				Aggregator:     spec.ContractAddress,
				RoundID:        roundID,
				NumSubmissions: 1,
			}, nil).Once()
		now := time.Now()
		pipelineORM.On("FindRun", int64(1)).Return(pipeline.Run{
			FinishedAt: &now,
		}, nil)

		fm.ExportedPollIfEligible(0, 0)

		fluxAggregator.AssertExpectations(t)
		logBroadcaster.AssertExpectations(t)
		keyStore.AssertExpectations(t)
		orm.AssertExpectations(t)
		pipelineRunner.AssertExpectations(t)
		contractSubmitter.AssertExpectations(t)
	})

	t.Run("when poll ticker fires, then NewRound log arrives", func(t *testing.T) {
		store, cleanup := cltest.NewStore(t)
		defer cleanup()

		_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
		oracles := []common.Address{nodeAddr, cltest.NewAddress()}

		var (
			spec              = NewSpecification()
			fluxAggregator    = new(mocks.FluxAggregator)
			logBroadcaster    = new(logmocks.Broadcaster)
			orm               = new(fmmocks.ORM)
			jobORM            = new(jobmocks.ORM)
			pipelineORM       = new(pipelinemocks.ORM)
			keyStore          = new(fmmocks.KeyStoreInterface)
			contractSubmitter = new(fmmocks.ContractSubmitter)
			pollTicker        = fluxmonitorv2.NewPollTicker(time.Minute, true)
			pipelineRunner    = new(pipelinemocks.Runner)
			pipelineSpec      = NewPipelineSpec()
			l                 = *logger.Default
			pipelineRun       = fluxmonitorv2.NewPipelineRun(
				pipelineRunner,
				pipelineSpec,
				spec.JobID,
				l,
			)

			paymentAmount  = store.Config.MinimumContractPayment().ToInt()
			availableFunds = big.NewInt(1).Mul(paymentAmount, big.NewInt(1000))
		)
		spec.PollTimerDisabled = true
		spec.IdleTimerDisabled = true

		const (
			roundID = 3
			answer  = 100
		)
		keyStore.On("Accounts").Return([]accounts.Account{{Address: nodeAddr}}).Once()

		fm, err := fluxmonitorv2.NewFluxMonitor(
			pipelineRun,
			orm,
			jobORM,
			pipelineORM,
			keyStore,
			pollTicker,
			fluxmonitorv2.NewPaymentChecker(assets.NewLink(1), nil),
			spec.ContractAddress,
			contractSubmitter,
			fluxmonitorv2.NewDeviationChecker(spec.Threshold, spec.AbsoluteThreshold),
			fluxmonitorv2.NewSubmissionChecker(big.NewInt(0), big.NewInt(100000000000), spec.Precision),
			fluxmonitorv2.Flags{},
			fluxAggregator,
			logBroadcaster,
			spec,
			func() {},
		)
		require.NoError(t, err)

		fm.OnConnect()

		// First, force the node to try to poll, which should result in a submission
		fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(0)).
			Return(flux_aggregator_wrapper.OracleRoundState{
				RoundId:          roundID,
				LatestSubmission: big.NewInt(answer),
				EligibleToSubmit: true,
				AvailableFunds:   availableFunds,
				PaymentAmount:    paymentAmount,
				OracleCount:      1,
			}, nil).
			Once()
		orm.
			On("FindOrCreateFluxMonitorRoundStats", spec.ContractAddress, uint32(roundID)).
			Return(fluxmonitorv2.FluxMonitorRoundStatsV2{
				Aggregator: spec.ContractAddress,
				RoundID:    roundID,
			}, nil).Once()
		pipelineRunner.
			On("ExecuteAndInsertNewRun", context.Background(), pipelineSpec, l).
			Return(int64(1), pipeline.FinalResult{
				Values: []interface{}{decimal.NewFromInt(answer)},
				Errors: []error{nil},
			}, nil).Once()
		contractSubmitter.On("Submit", big.NewInt(roundID), big.NewInt(answer)).Return(nil).Once()
		orm.
			On("UpdateFluxMonitorRoundStats",
				spec.ContractAddress,
				uint32(roundID),
				int64(1),
			).
			Return(nil).
			Once()

		fluxAggregator.On("Address").Return(spec.ContractAddress)
		fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)
		fm.SetOracleAddress()
		fm.ExportedPollIfEligible(0, 0)

		// Now fire off the NewRound log and ensure it does not respond this time
		orm.On("MostRecentFluxMonitorRoundID", spec.ContractAddress).Return(uint32(roundID), nil)
		orm.
			On("FindOrCreateFluxMonitorRoundStats", spec.ContractAddress, uint32(roundID)).
			Return(fluxmonitorv2.FluxMonitorRoundStatsV2{
				PipelineRunID:  corenull.NewInt64(int64(1), true),
				Aggregator:     spec.ContractAddress,
				RoundID:        roundID,
				NumSubmissions: 1,
			}, nil).Once()
		pipelineORM.On("FindRun", int64(1)).Return(pipeline.Run{}, nil)

		fm.ExportedRespondToNewRoundLog(&flux_aggregator_wrapper.FluxAggregatorNewRound{
			RoundId:   big.NewInt(roundID),
			StartedAt: big.NewInt(0),
		})

		fluxAggregator.AssertExpectations(t)
		logBroadcaster.AssertExpectations(t)
		keyStore.AssertExpectations(t)
		orm.AssertExpectations(t)
		pipelineRunner.AssertExpectations(t)
		contractSubmitter.AssertExpectations(t)
	})
}
