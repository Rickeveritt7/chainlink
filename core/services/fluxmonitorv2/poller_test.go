package fluxmonitorv2_test

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/onsi/gomega"
	"github.com/pkg/errors"
	"github.com/shopspring/decimal"
	"github.com/smartcontractkit/chainlink/core/assets"
	"github.com/smartcontractkit/chainlink/core/internal/cltest"
	"github.com/smartcontractkit/chainlink/core/internal/gethwrappers/generated/flux_aggregator_wrapper"
	"github.com/smartcontractkit/chainlink/core/internal/mocks"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services/eth"
	"github.com/smartcontractkit/chainlink/core/services/eth/contracts"
	"github.com/smartcontractkit/chainlink/core/services/fluxmonitor"
	"github.com/smartcontractkit/chainlink/core/services/fluxmonitorv2"
	jobmocks "github.com/smartcontractkit/chainlink/core/services/job/mocks"
	logmocks "github.com/smartcontractkit/chainlink/core/services/log/mocks"
	"github.com/smartcontractkit/chainlink/core/services/pipeline"
	pipelinemocks "github.com/smartcontractkit/chainlink/core/services/pipeline/mocks"
	"github.com/smartcontractkit/chainlink/core/store/models"
	"github.com/smartcontractkit/chainlink/core/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const oracleCount uint8 = 17

type answerSet struct{ latestAnswer, polledAnswer int64 }

var (
	submitHash     = utils.MustHash("submit(uint256,int256)")
	submitSelector = submitHash[:4]
	now            = func() uint64 { return uint64(time.Now().UTC().Unix()) }
	nilOpts        *bind.CallOpts

	makeRoundDataForRoundID = func(roundID uint32) flux_aggregator_wrapper.LatestRoundData {
		return flux_aggregator_wrapper.LatestRoundData{
			RoundId: big.NewInt(int64(roundID)),
		}
	}
	freshContractRoundDataResponse = func() (flux_aggregator_wrapper.LatestRoundData, error) {
		return flux_aggregator_wrapper.LatestRoundData{}, errors.New("unstarted")
	}
)

func NewSpecification() fluxmonitorv2.Specification {
	return fluxmonitorv2.Specification{
		ID:                "1",
		JobID:             1,
		ContractAddress:   cltest.NewEIP55Address(),
		Precision:         2,
		Threshold:         0.5,
		AbsoluteThreshold: 0.01,
		PollTimerPeriod:   time.Minute,
		PollTimerDisabled: false,
		IdleTimerPeriod:   time.Minute,
		IdleTimerDisabled: false,
	}
}

func NewPipelineRun() fluxmonitorv2.PipelineRun {
	jobID := int32(1)
	pipelineRunner := new(pipelinemocks.Runner)
	pipelineSpec := pipeline.Spec{
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
	l := *logger.Default

	return fluxmonitorv2.NewPipelineRun(
		pipelineRunner,
		pipelineSpec,
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
		previousRunStatus models.RunStatus
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
			hasPreviousRun: true, previousRunStatus: models.RunStatusCompleted,
			expectedToPoll: false, expectedToSubmit: false,
		}, {
			name:     "previous job run in progress",
			eligible: true, connected: true, funded: true, answersDeviate: true,
			hasPreviousRun: true, previousRunStatus: models.RunStatusInProgress,
			expectedToPoll: false, expectedToSubmit: false,
		}, {
			name:     "previous job run cancelled",
			eligible: true, connected: true, funded: true, answersDeviate: true,
			hasPreviousRun: true, previousRunStatus: models.RunStatusCancelled,
			expectedToPoll: false, expectedToSubmit: false,
		}, {
			name:     "previous job run errored",
			eligible: true, connected: true, funded: true, answersDeviate: true,
			hasPreviousRun: true, previousRunStatus: models.RunStatusErrored,
			expectedToPoll: true, expectedToSubmit: true,
		},
	}

	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)

	const reportableRoundID = 2
	thresholds := struct{ abs, rel float64 }{0.1, 200}
	deviatedAnswers := answerSet{1, 100}
	undeviatedAnswers := answerSet{100, 101}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			answers := undeviatedAnswers
			if tc.answersDeviate {
				answers = deviatedAnswers
			}

			fluxAggregator := new(mocks.FluxAggregator)
			logBroadcaster := new(logmocks.Broadcaster)

			spec := fluxmonitorv2.Specification{
				ID:                "1",
				JobID:             1,
				ContractAddress:   cltest.NewEIP55Address(),
				Precision:         2,
				Threshold:         0.5,
				AbsoluteThreshold: 0.01,
				PollTimerPeriod:   time.Minute,
				PollTimerDisabled: false,
				IdleTimerPeriod:   time.Minute,
				IdleTimerDisabled: false,
			}

			// if test.hasPreviousRun {
			// 	run := cltest.NewJobRun(job)
			// 	run.Status = test.previousRunStatus
			// 	require.NoError(t, store.CreateJobRun(&run))
			// 	_, err := store.FindOrCreateFluxMonitorRoundStats(initr.Address, reportableRoundID)
			// 	require.NoError(t, err)
			// 	store.UpdateFluxMonitorRoundStats(initr.Address, reportableRoundID, run.ID)
			// }

			latestAnswerNoPrecision := answers.latestAnswer * int64(
				math.Pow10(int(spec.Precision)),
			)

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
			fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(0)).Return(roundState, nil).Maybe()

			pipelineRunner := new(pipelinemocks.Runner)
			pipelineSpec := pipeline.Spec{
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
			l := *logger.Default

			pipelineRun := fluxmonitorv2.NewPipelineRun(
				pipelineRunner,
				pipelineSpec,
				spec.JobID,
				l,
			)

			// if test.expectedToPoll {
			// fetcher.On("Fetch", mock.Anything, mock.Anything).Return(decimal.NewFromInt(answers.polledAnswer), nil)

			pipelineRunner.
				On("ExecuteAndInsertNewRun", context.Background(), pipelineSpec, l).
				Return(pipeline.FinalResult{
					Values: []interface{}{1.1},
					Errors: []error{nil},
				}, nil)
			// }

			if tc.expectedToSubmit {
				fmt.Println("Submitting")
				// run := cltest.NewJobRun(job)
				// require.NoError(t, store.CreateJobRun(&run))

				// data, err := models.ParseJSON([]byte(fmt.Sprintf(`{
				// 	"result": "%d",
				// 	"address": "%s",
				// 	"functionSelector": "0x%x",
				// 	"dataPrefix": "0x000000000000000000000000000000000000000000000000000000000000000%d"
				// }`, answers.polledAnswer, spec.ContractAddress.Hex(), submitSelector, reportableRoundID)))
				// require.NoError(t, err)

				// rm.On("Create", spec.JobID, &initr, mock.Anything, mock.MatchedBy(func(runRequest *models.RunRequest) bool {
				// 	return reflect.DeepEqual(runRequest.RequestParams.Result.Value(), data.Result.Value())
				// })).Return(&run, nil)
			}

			jobORM := new(jobmocks.ORM)

			checker, err := fluxmonitorv2.NewFluxMonitor(
				pipelineRun,
				fluxmonitorv2.Config{
					DefaultHTTPTimeout:   time.Minute,
					FlagsContractAddress: "",
					MinContractPayment:   assets.NewLink(1),
				},
				fluxmonitorv2.NewDatabase(store, jobORM),
				fluxAggregator,
				logBroadcaster,
				spec,
				nil,
				nil,
				nil,
				func() {},
				big.NewInt(0),
				big.NewInt(100000000000),
			)
			require.NoError(t, err)

			if tc.connected {
				checker.OnConnect()
			}
			oracles := []common.Address{nodeAddr, cltest.NewAddress()}
			fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)
			checker.SetOracleAddress()

			checker.ExportedPollIfEligible(thresholds.rel, thresholds.abs)

			fluxAggregator.AssertExpectations(t)
		})
	}
}

// func TestPollingDeviationChecker_BuffersLogs(t *testing.T) {
// 	store, cleanup := cltest.NewStore(t)
// 	defer cleanup()

// 	_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
// 	oracles := []common.Address{nodeAddr, cltest.NewAddress()}

// 	const (
// 		fetchedValue = 100
// 	)

// 	jobORM := new(jobmocks.ORM)
// 	spec := NewSpecification()
// 	spec.PollTimerDisabled = true
// 	spec.IdleTimerDisabled = true

// 	// job := cltest.NewJobWithFluxMonitorInitiator()
// 	// initr := job.Initiators[0]
// 	// initr.ID = 1
// 	// initr.PollTimer.Disabled = true
// 	// initr.IdleTimer.Disabled = true
// 	// require.NoError(t, store.CreateJob(&job))

// 	// Test helpers
// 	var (
// 		makeRoundStateForRoundID = func(roundID uint32) flux_aggregator_wrapper.OracleRoundState {
// 			return flux_aggregator_wrapper.OracleRoundState{
// 				RoundId:          roundID,
// 				EligibleToSubmit: true,
// 				LatestSubmission: big.NewInt(100 * int64(math.Pow10(int(spec.Precision)))),
// 				AvailableFunds:   store.Config.MinimumContractPayment().ToInt(),
// 				PaymentAmount:    store.Config.MinimumContractPayment().ToInt(),
// 			}
// 		}

// 		matchRunRequestForRoundID = func(roundID uint32) interface{} {
// 			data, err := models.ParseJSON([]byte(fmt.Sprintf(`{
//                 "result": "%d",
//                 "address": "%s",
//                 "functionSelector": "0x%x",
//                 "dataPrefix": "0x000000000000000000000000000000000000000000000000000000000000000%d"
//             }`, fetchedValue, spec.ContractAddress.Hex(), submitSelector, roundID)))
// 			require.NoError(t, err)

// 			return mock.MatchedBy(func(runRequest *models.RunRequest) bool {
// 				return reflect.DeepEqual(runRequest.RequestParams.Result.Value(), data.Result.Value())
// 			})
// 		}
// 	)

// 	chBlock := make(chan struct{})
// 	chSafeToAssert := make(chan struct{})
// 	chSafeToFillQueue := make(chan struct{})

// 	fluxAggregator := new(mocks.FluxAggregator)
// 	fluxAggregator.On("LatestRoundData", nilOpts).Return(freshContractRoundDataResponse()).Once()
// 	fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(1)).
// 		Return(makeRoundStateForRoundID(1), nil).
// 		Run(func(mock.Arguments) {
// 			close(chSafeToFillQueue)
// 			<-chBlock
// 		}).
// 		Once()
// 	fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(3)).Return(makeRoundStateForRoundID(3), nil).Once()
// 	fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(4)).Return(makeRoundStateForRoundID(4), nil).Once()
// 	fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)
// 	fluxAggregator.On("Address").Return(spec.ContractAddress.Address(), nil)

// 	fetcher := new(mocks.Fetcher)
// 	fetcher.On("Fetch", mock.Anything, mock.Anything).Return(decimal.NewFromInt(fetchedValue), nil)

// 	logBroadcaster := new(logmocks.Broadcaster)
// 	logBroadcaster.On("Register", spec.ContractAddress.Address(), mock.Anything).Return(true)
// 	logBroadcaster.On("Unregister", spec.ContractAddress.Address(), mock.Anything)

// 	rm := new(mocks.RunManager)
// 	run := cltest.NewJobRun(job)
// 	require.NoError(t, store.CreateJobRun(&run))

// 	rm.On("Create", job.ID, &initr, mock.Anything, matchRunRequestForRoundID(1)).Return(&run, nil).Once()
// 	rm.On("Create", job.ID, &initr, mock.Anything, matchRunRequestForRoundID(3)).Return(&run, nil).Once()
// 	rm.On("Create", job.ID, &initr, mock.Anything, matchRunRequestForRoundID(4)).Return(&run, nil).Once().
// 		Run(func(mock.Arguments) { close(chSafeToAssert) })

// 	checker, err := fluxmonitor.NewPollingDeviationChecker(
// 		store,
// 		fluxAggregator,
// 		logBroadcaster,
// 		initr,
// 		nil,
// 		rm,
// 		fetcher,
// 		nil,
// 		func() {},
// 		big.NewInt(0),
// 		big.NewInt(100000000000),
// 	)
// 	require.NoError(t, err)

// 	checker.OnConnect()
// 	checker.Start()

// 	var logBroadcasts []*logmocks.Broadcast

// 	for i := 1; i <= 4; i++ {
// 		logBroadcast := new(logmocks.Broadcast)
// 		logBroadcast.On("DecodedLog").Return(&flux_aggregator_wrapper.FluxAggregatorNewRound{RoundId: big.NewInt(int64(i)), StartedAt: big.NewInt(0)})
// 		logBroadcast.On("WasAlreadyConsumed").Return(false, nil)
// 		logBroadcast.On("MarkConsumed").Return(nil)
// 		logBroadcasts = append(logBroadcasts, logBroadcast)
// 	}

// 	checker.HandleLog(logBroadcasts[0], nil) // Get the checker to start processing a log so we can freeze it
// 	<-chSafeToFillQueue
// 	checker.HandleLog(logBroadcasts[1], nil) // This log is evicted from the priority queue
// 	checker.HandleLog(logBroadcasts[2], nil)
// 	checker.HandleLog(logBroadcasts[3], nil)

// 	close(chBlock)
// 	<-chSafeToAssert

// 	fluxAggregator.AssertExpectations(t)
// }

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

			fluxAggregator := new(mocks.FluxAggregator)
			logBroadcast := new(logmocks.Broadcast)
			logBroadcaster := new(logmocks.Broadcaster)

			jobORM := new(jobmocks.ORM)
			spec := NewSpecification()
			spec.PollTimerDisabled = true
			spec.IdleTimerDisabled = tc.idleTimerDisabled
			spec.IdleTimerPeriod = tc.idleDuration

			const fetchedAnswer = 100
			answerBigInt := big.NewInt(fetchedAnswer * int64(math.Pow10(int(spec.Precision))))

			// fluxAggregator.On("SubscribeToLogs", mock.Anything).Return(true, contracts.UnsubscribeFunc(func() {}), nil)
			fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)
			fluxAggregator.On("Address").Return(spec.ContractAddress.Address())
			logBroadcaster.On("Register", spec.ContractAddress.Address(), mock.Anything).Return(true)
			logBroadcaster.On("Unregister", spec.ContractAddress.Address(), mock.Anything)

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
				fluxmonitorv2.Config{
					DefaultHTTPTimeout:   time.Minute,
					FlagsContractAddress: "",
					MinContractPayment:   assets.NewLink(1),
				},
				fluxmonitorv2.NewDatabase(store, jobORM),
				fluxAggregator,
				logBroadcaster,
				spec,
				nil,
				nil,
				nil,
				func() {},
				big.NewInt(0),
				big.NewInt(100000000000),
			)
			require.NoError(t, err)

			// deviationChecker, err := fluxmonitor.NewPollingDeviationChecker(
			// 	store,
			// 	fluxAggregator,
			// 	logBroadcaster,
			// 	initr,
			// 	nil,
			// 	runManager,
			// 	fetcher,
			// 	nil,
			// 	func() {},
			// 	big.NewInt(0),
			// 	big.NewInt(100000000000),
			// )
			// require.NoError(t, err)

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
		})
	}
}

func TestFluxMonitor_RoundTimeoutCausesPoll_timesOutAtZero(t *testing.T) {
	t.Parallel()

	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
	oracles := []common.Address{nodeAddr, cltest.NewAddress()}

	fluxAggregator := new(mocks.FluxAggregator)
	logBroadcaster := new(logmocks.Broadcaster)
	jobORM := new(jobmocks.ORM)
	spec := NewSpecification()
	spec.PollTimerDisabled = true
	spec.IdleTimerDisabled = true

	ch := make(chan struct{})

	const fetchedAnswer = 100
	answerBigInt := big.NewInt(fetchedAnswer * int64(math.Pow10(int(spec.Precision))))
	logBroadcaster.On("Register", spec.ContractAddress.Address(), mock.Anything).Return(true)
	logBroadcaster.On("Unregister", spec.ContractAddress.Address(), mock.Anything)

	fluxAggregator.On("LatestRoundData", nilOpts).Return(makeRoundDataForRoundID(1), nil).Once()
	fluxAggregator.On("Address").Return(spec.ContractAddress.Address())
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
		fluxmonitorv2.Config{
			DefaultHTTPTimeout:   time.Minute,
			FlagsContractAddress: "",
			MinContractPayment:   assets.NewLink(1),
		},
		fluxmonitorv2.NewDatabase(store, jobORM),
		fluxAggregator,
		logBroadcaster,
		spec,
		nil,
		nil,
		nil,
		func() {},
		big.NewInt(0),
		big.NewInt(100000000000),
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
}

func TestFluxMonitor_UsesPreviousRoundStateOnStartup_RoundTimeout(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
	oracles := []common.Address{nodeAddr, cltest.NewAddress()}

	logBroadcaster := new(logmocks.Broadcaster)
	jobORM := new(jobmocks.ORM)
	spec := NewSpecification()
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

			fluxAggregator := new(mocks.FluxAggregator)

			logBroadcaster.On("Register", spec.ContractAddress.Address(), mock.Anything).Return(true)
			logBroadcaster.On("Unregister", spec.ContractAddress.Address(), mock.Anything)

			fluxAggregator.On("Address").Return(spec.ContractAddress.Address())
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
				fluxmonitorv2.Config{
					DefaultHTTPTimeout:   time.Minute,
					FlagsContractAddress: "",
					MinContractPayment:   assets.NewLink(1),
				},
				fluxmonitorv2.NewDatabase(store, jobORM),
				fluxAggregator,
				logBroadcaster,
				spec,
				nil,
				nil,
				nil,
				func() {},
				big.NewInt(0),
				big.NewInt(100000000000),
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
		})
	}
}

func TestFluxMonitor_UsesPreviousRoundStateOnStartup_IdleTimer(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
	oracles := []common.Address{nodeAddr, cltest.NewAddress()}

	logBroadcaster := new(logmocks.Broadcaster)
	jobORM := new(jobmocks.ORM)
	spec := NewSpecification()
	spec.PollTimerDisabled = true
	spec.IdleTimerDisabled = false

	almostExpired := time.Now().
		Add(spec.IdleTimerPeriod * -1).
		Add(2 * time.Second).
		Unix()

	tests := []struct {
		name             string
		startedAt        uint64
		expectedToSubmit bool
	}{
		{"active round exists - idleTimer about to expired", uint64(almostExpired), true},
		{"active round exists - idleTimer will not expire", 100, false},
		{"no active round", 0, false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			fluxAggregator := new(mocks.FluxAggregator)

			logBroadcaster.On("Register", spec.ContractAddress.Address(), mock.Anything).Return(true)
			logBroadcaster.On("Unregister", spec.ContractAddress.Address(), mock.Anything)

			fluxAggregator.On("Address").Return(spec.ContractAddress.Address())
			fluxAggregator.On("GetOracles", nilOpts).Return(oracles, nil)
			fluxAggregator.On("LatestRoundData", nilOpts).Return(makeRoundDataForRoundID(1), nil).Once()
			// first roundstate in setInitialTickers()
			fluxAggregator.On("OracleRoundState", nilOpts, nodeAddr, uint32(1)).Return(flux_aggregator_wrapper.OracleRoundState{
				RoundId:          1,
				EligibleToSubmit: false,
				StartedAt:        test.startedAt,
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
				fluxmonitorv2.Config{
					DefaultHTTPTimeout:   time.Minute,
					FlagsContractAddress: "",
					MinContractPayment:   assets.NewLink(1),
				},
				fluxmonitorv2.NewDatabase(store, jobORM),
				fluxAggregator,
				logBroadcaster,
				spec,
				nil,
				nil,
				nil,
				func() {},
				big.NewInt(0),
				big.NewInt(100000000000),
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
		})
	}
}

func TestFluxMonitor_RoundTimeoutCausesPoll_timesOutNotZero(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	_, nodeAddr := cltest.MustAddRandomKeyToKeystore(t, store)
	oracles := []common.Address{nodeAddr, cltest.NewAddress()}

	fluxAggregator := new(mocks.FluxAggregator)
	logBroadcaster := new(logmocks.Broadcaster)
	jobORM := new(jobmocks.ORM)
	spec := NewSpecification()
	spec.PollTimerDisabled = true
	spec.IdleTimerDisabled = true

	const fetchedAnswer = 100
	answerBigInt := big.NewInt(fetchedAnswer * int64(math.Pow10(int(spec.Precision))))

	chRoundState1 := make(chan struct{})
	chRoundState2 := make(chan struct{})

	logBroadcaster.On("Register", spec.ContractAddress.Address(), mock.Anything).Return(true)
	logBroadcaster.On("Unregister", spec.ContractAddress.Address(), mock.Anything)

	fluxAggregator.On("Address").Return(spec.ContractAddress.Address())
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
		fluxmonitorv2.Config{
			DefaultHTTPTimeout:   time.Minute,
			FlagsContractAddress: "",
			MinContractPayment:   assets.NewLink(1),
		},
		fluxmonitorv2.NewDatabase(store, jobORM),
		fluxAggregator,
		logBroadcaster,
		spec,
		nil,
		nil,
		nil,
		func() {},
		big.NewInt(0),
		big.NewInt(100000000000),
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

	// fetcher.AssertExpectations(t)
	// runManager.AssertExpectations(t)
	fluxAggregator.AssertExpectations(t)
}

func TestFluxMonitor_SufficientFunds(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	fluxAggregator := new(mocks.FluxAggregator)
	logBroadcaster := new(logmocks.Broadcaster)
	jobORM := new(jobmocks.ORM)

	checker, err := fluxmonitorv2.NewFluxMonitor(
		NewPipelineRun(),
		fluxmonitorv2.Config{
			DefaultHTTPTimeout:   time.Minute,
			FlagsContractAddress: "",
			MinContractPayment:   assets.NewLink(1),
		},
		fluxmonitorv2.NewDatabase(store, jobORM),
		fluxAggregator,
		logBroadcaster,
		NewSpecification(),
		nil,
		nil,
		nil,
		func() {},
		big.NewInt(0),
		big.NewInt(100000000000),
	)
	require.NoError(t, err)

	payment := 100
	rounds := 3
	oracleCount := 21
	min := payment * rounds * oracleCount

	testCases := []struct {
		name  string
		funds int
		want  bool
	}{
		{"above minimum", min + 1, true},
		{"equal to minimum", min, true},
		{"below minimum", min - 1, false},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state := flux_aggregator_wrapper.OracleRoundState{
				AvailableFunds: big.NewInt(int64(tc.funds)),
				PaymentAmount:  big.NewInt(int64(payment)),
				OracleCount:    uint8(oracleCount),
			}
			assert.Equal(t, tc.want, checker.ExportedSufficientFunds(state))
		})
	}
}

func TestFluxMonitor_SufficientPayment(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	fluxAggregator := new(mocks.FluxAggregator)
	logBroadcaster := new(logmocks.Broadcaster)
	jobORM := new(jobmocks.ORM)

	var payment int64 = 10
	var eq = payment
	var gt int64 = payment + 1
	var lt int64 = payment - 1

	tests := []struct {
		name               string
		minContractPayment int64
		minJobPayment      interface{} // nil or int64
		want               bool
	}{
		{"payment above min contract payment, no min job payment", lt, nil, true},
		{"payment equal to min contract payment, no min job payment", eq, nil, true},
		{"payment below min contract payment, no min job payment", gt, nil, false},

		{"payment above min contract payment, above min job payment", lt, lt, true},
		{"payment equal to min contract payment, above min job payment", eq, lt, true},
		{"payment below min contract payment, above min job payment", gt, lt, false},

		{"payment above min contract payment, equal to min job payment", lt, eq, true},
		{"payment equal to min contract payment, equal to min job payment", eq, eq, true},
		{"payment below min contract payment, equal to min job payment", gt, eq, false},

		{"payment above minimum contract payment, below min job payment", lt, gt, false},
		{"payment equal to minimum contract payment, below min job payment", eq, gt, false},
		{"payment below minimum contract payment, below min job payment", gt, gt, false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var minJobPayment *assets.Link

			if tc.minJobPayment != nil {
				mjb := assets.Link(*big.NewInt(tc.minJobPayment.(int64)))
				minJobPayment = &mjb
			}

			fm, err := fluxmonitorv2.NewFluxMonitor(
				NewPipelineRun(),
				fluxmonitorv2.Config{
					DefaultHTTPTimeout:   time.Minute,
					FlagsContractAddress: "",
					MinContractPayment:   assets.NewLink(tc.minContractPayment),
				},
				fluxmonitorv2.NewDatabase(store, jobORM),
				fluxAggregator,
				logBroadcaster,
				NewSpecification(),
				nil,
				minJobPayment,
				nil,
				func() {},
				big.NewInt(0),
				big.NewInt(100000000000),
			)
			require.NoError(t, err)

			assert.Equal(t, tc.want, fm.ExportedSufficientPayment(big.NewInt(payment)))
		})
	}
}

func TestFluxMonitor_IsFlagLowered(t *testing.T) {
	falseFalse := "0x0000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
	falseTrue := "0x0000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000001"
	trueFalse := "0x0000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000000"
	trueTrue := "0x0000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000001"

	testCases := []struct {
		name           string
		getFlagsResult string
		expected       bool
	}{
		{"both lowered", falseFalse, true},
		{"global lowered", falseTrue, true},
		{"contract lowered", trueFalse, true},
		{"both raised", trueTrue, false},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, cleanup := cltest.NewStore(t)
			defer cleanup()

			gethClient := new(mocks.GethClient)
			defer gethClient.AssertExpectations(t)

			ethClient := eth.NewClientWith(nil, gethClient)
			fluxAggregator := new(mocks.FluxAggregator)
			logBroadcaster := new(logmocks.Broadcaster)
			jobORM := new(jobmocks.ORM)
			spec := NewSpecification()

			flagsContractAddress := cltest.NewAddress()
			flagsContract, err := contracts.NewFlagsContract(flagsContractAddress, ethClient)
			require.NoError(t, err)

			getFlagsResultBytes, err := hexutil.Decode(tc.getFlagsResult)
			require.NoError(t, err)

			gethClient.On("CallContract", mock.Anything, mock.Anything, mock.Anything).
				Run(func(args mock.Arguments) {
					payload := args.Get(1).(ethereum.CallMsg).Data[4:] // omit signature bytes
					address1 := common.BytesToAddress(payload[64:96])  // first address
					address2 := common.BytesToAddress(payload[96:])    // second address
					require.Equal(t, utils.ZeroAddress, address1)
					require.Equal(t, spec.ContractAddress.Address(), address2)
				}).
				Return(getFlagsResultBytes, nil)

			fm, err := fluxmonitorv2.NewFluxMonitor(
				NewPipelineRun(),
				fluxmonitorv2.Config{
					DefaultHTTPTimeout:   time.Minute,
					FlagsContractAddress: "",
					MinContractPayment:   assets.NewLink(1),
				},
				fluxmonitorv2.NewDatabase(store, jobORM),
				fluxAggregator,
				logBroadcaster,
				spec,
				ethClient,
				nil,
				flagsContract,
				func() {},
				big.NewInt(0),
				big.NewInt(100000000000),
			)
			require.NoError(t, err)

			result, err := fm.ExportedIsFlagLowered()
			require.NoError(t, err)
			require.Equal(t, tc.expected, result)
		})
	}
}

func TestFluxMonitor_HandlesNilLogs(t *testing.T) {
	t.Parallel()

	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	fluxAggregator := new(mocks.FluxAggregator)
	logBroadcaster := new(logmocks.Broadcaster)
	jobORM := new(jobmocks.ORM)
	spec := NewSpecification()

	fm, err := fluxmonitorv2.NewFluxMonitor(
		NewPipelineRun(),
		fluxmonitorv2.Config{
			DefaultHTTPTimeout:   time.Minute,
			FlagsContractAddress: "",
			MinContractPayment:   assets.NewLink(1),
		},
		fluxmonitorv2.NewDatabase(store, jobORM),
		fluxAggregator,
		logBroadcaster,
		spec,
		nil,
		nil,
		nil,
		func() {},
		big.NewInt(0),
		big.NewInt(100000000000),
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
}

func TestFluxMonitor_ConsumeLogBroadcast(t *testing.T) {
	t.Parallel()

	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	fluxAggregator := new(mocks.FluxAggregator)
	logBroadcaster := new(logmocks.Broadcaster)
	jobORM := new(jobmocks.ORM)
	spec := NewSpecification()

	fm, err := fluxmonitorv2.NewFluxMonitor(
		NewPipelineRun(),
		fluxmonitorv2.Config{
			DefaultHTTPTimeout:   time.Minute,
			FlagsContractAddress: "",
			MinContractPayment:   assets.NewLink(1),
		},
		fluxmonitorv2.NewDatabase(store, jobORM),
		fluxAggregator,
		logBroadcaster,
		spec,
		nil,
		nil,
		nil,
		func() {},
		big.NewInt(0),
		big.NewInt(100000000000),
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

	fm.ExportedBacklog().Add(fluxmonitor.PriorityNewRoundLog, logBroadcast)
	fm.ExportedProcessLogs()

	logBroadcast.AssertExpectations(t)
}

func TestFluxMonitor_ConsumeLogBroadcast_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		consumed bool
		err      error
	}{
		{"already consumed", true, nil},
		{"error determining already consumed", false, errors.New("err")},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			store, cleanup := cltest.NewStore(t)
			defer cleanup()

			fluxAggregator := new(mocks.FluxAggregator)
			logBroadcaster := new(logmocks.Broadcaster)
			jobORM := new(jobmocks.ORM)
			spec := NewSpecification()

			fm, err := fluxmonitorv2.NewFluxMonitor(
				NewPipelineRun(),
				fluxmonitorv2.Config{
					DefaultHTTPTimeout:   time.Minute,
					FlagsContractAddress: "",
					MinContractPayment:   assets.NewLink(1),
				},
				fluxmonitorv2.NewDatabase(store, jobORM),
				fluxAggregator,
				logBroadcaster,
				spec,
				nil,
				nil,
				nil,
				func() {},
				big.NewInt(0),
				big.NewInt(100000000000),
			)
			require.NoError(t, err)

			logBroadcast := new(logmocks.Broadcast)
			logBroadcast.On("WasAlreadyConsumed").Return(test.consumed, test.err).Once()

			fm.ExportedBacklog().Add(fluxmonitor.PriorityNewRoundLog, logBroadcast)
			fm.ExportedProcessLogs()

			logBroadcast.AssertExpectations(t)
		})
	}
}

//------------------------------------------------------------------------------

type outsideDeviationRow struct {
	name                string
	curPrice, nextPrice decimal.Decimal
	threshold           float64 // in percentage
	absoluteThreshold   float64
	expectation         bool
}

func (o outsideDeviationRow) String() string {
	return fmt.Sprintf(
		`{name: "%s", curPrice: %s, nextPrice: %s, threshold: %.2f, `+
			"absoluteThreshold: %f, expectation: %v}", o.name, o.curPrice, o.nextPrice,
		o.threshold, o.absoluteThreshold, o.expectation)
}

func TestOutsideDeviation(t *testing.T) {
	t.Parallel()
	f, i := decimal.NewFromFloat, decimal.NewFromInt
	tests := []outsideDeviationRow{
		// Start with a huge absoluteThreshold, to test relative threshold behavior
		{"0 current price, outside deviation", i(0), i(100), 2, 0, true},
		{"0 current and next price", i(0), i(0), 2, 0, false},

		{"inside deviation", i(100), i(101), 2, 0, false},
		{"equal to deviation", i(100), i(102), 2, 0, true},
		{"outside deviation", i(100), i(103), 2, 0, true},
		{"outside deviation zero", i(100), i(0), 2, 0, true},

		{"inside deviation, crosses 0 backwards", f(0.1), f(-0.1), 201, 0, false},
		{"equal to deviation, crosses 0 backwards", f(0.1), f(-0.1), 200, 0, true},
		{"outside deviation, crosses 0 backwards", f(0.1), f(-0.1), 199, 0, true},

		{"inside deviation, crosses 0 forwards", f(-0.1), f(0.1), 201, 0, false},
		{"equal to deviation, crosses 0 forwards", f(-0.1), f(0.1), 200, 0, true},
		{"outside deviation, crosses 0 forwards", f(-0.1), f(0.1), 199, 0, true},

		{"thresholds=0, deviation", i(0), i(100), 0, 0, true},
		{"thresholds=0, no deviation", i(100), i(100), 0, 0, true},
		{"thresholds=0, all zeros", i(0), i(0), 0, 0, true},
	}

	c := func(test outsideDeviationRow) {
		actual := fluxmonitor.OutsideDeviation(test.curPrice, test.nextPrice,
			fluxmonitor.DeviationThresholds{Rel: test.threshold,
				Abs: test.absoluteThreshold})
		assert.Equal(t, test.expectation, actual,
			"check on OutsideDeviation failed for %s", test)
	}

	for _, test := range tests {
		test := test
		// Checks on relative threshold
		t.Run(test.name, func(t *testing.T) { c(test) })
		// Check corresponding absolute threshold tests; make relative threshold
		// always pass (as long as curPrice and nextPrice aren't both 0.)
		test2 := test
		test2.threshold = 0
		// absoluteThreshold is initially zero, so any change will trigger
		test2.expectation = test2.curPrice.Sub(test.nextPrice).Abs().GreaterThan(i(0)) ||
			test2.absoluteThreshold == 0
		t.Run(test.name+" threshold zeroed", func(t *testing.T) { c(test2) })
		// Huge absoluteThreshold means trigger always fails
		test3 := test
		test3.absoluteThreshold = 1e307
		test3.expectation = false
		t.Run(test.name+" max absolute threshold", func(t *testing.T) { c(test3) })
	}
}
