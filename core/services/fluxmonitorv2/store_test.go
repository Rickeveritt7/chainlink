package fluxmonitorv2_test

import (
	"context"
	"testing"
	"time"

	"github.com/smartcontractkit/chainlink/core/internal/cltest"
	"github.com/smartcontractkit/chainlink/core/services/fluxmonitorv2"
	"github.com/smartcontractkit/chainlink/core/services/job"
	jobmocks "github.com/smartcontractkit/chainlink/core/services/job/mocks"
	"github.com/smartcontractkit/chainlink/core/services/pipeline"
	pipelinemocks "github.com/smartcontractkit/chainlink/core/services/pipeline/mocks"
	"github.com/smartcontractkit/chainlink/core/services/postgres"
	"github.com/stretchr/testify/require"
)

func TestStore_RecordError(t *testing.T) {
	t.Parallel()

	corestore, cleanup := cltest.NewStore(t)
	defer cleanup()

	jobORM := new(jobmocks.ORM)
	pipelineORM := new(pipelinemocks.ORM)
	store := fluxmonitorv2.NewStore(corestore.DB, corestore, jobORM, pipelineORM)

	jobID := int32(1)
	description := "Testing error"

	jobORM.On("RecordError", context.Background(), jobID, description).Once()

	store.RecordError(jobID, description)

	jobORM.AssertExpectations(t)
}

func TestStore_KeyStoreAccounts(t *testing.T) {
	t.Parallel()

	corestore, cleanup := cltest.NewStore(t)
	defer cleanup()

	jobORM := new(jobmocks.ORM)
	pipelineORM := new(pipelinemocks.ORM)
	store := fluxmonitorv2.NewStore(corestore.DB, corestore, jobORM, pipelineORM)

	corestore.KeyStore.Unlock(cltest.Password)
	account, err := corestore.KeyStore.NewAccount()
	require.NoError(t, err)

	accounts := store.KeyStoreAccounts()
	require.Len(t, accounts, 1)
	require.Equal(t, account, accounts[0])
}

func TestStore_FindOrCreateFluxMonitorRoundStats(t *testing.T) {
	t.Parallel()

	corestore, cleanup := cltest.NewStore(t)
	defer cleanup()

	jobORM := new(jobmocks.ORM)
	pipelineORM := new(pipelinemocks.ORM)
	store := fluxmonitorv2.NewStore(corestore.DB, corestore, jobORM, pipelineORM)

	address := cltest.NewAddress()
	var roundID uint32 = 1

	stats, err := store.FindOrCreateFluxMonitorRoundStats(address, roundID)
	require.NoError(t, err)
	require.Equal(t, roundID, stats.RoundID)
	require.Equal(t, address, stats.Aggregator)

	count, err := store.CountFluxMonitorRoundStats()
	require.NoError(t, err)
	require.Equal(t, 1, count)

	stats, err = store.FindOrCreateFluxMonitorRoundStats(address, roundID)
	require.NoError(t, err)
	require.Equal(t, roundID, stats.RoundID)
	require.Equal(t, address, stats.Aggregator)

	count, err = store.CountFluxMonitorRoundStats()
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestStore_MostRecentFluxMonitorRoundID(t *testing.T) {
	t.Parallel()

	corestore, cleanup := cltest.NewStore(t)
	defer cleanup()
	jobORM := new(jobmocks.ORM)
	pipelineORM := new(pipelinemocks.ORM)
	store := fluxmonitorv2.NewStore(corestore.DB, corestore, jobORM, pipelineORM)

	address := cltest.NewAddress()

	for round := uint32(0); round < 10; round++ {
		_, err := store.FindOrCreateFluxMonitorRoundStats(address, round)
		require.NoError(t, err)
	}

	count, err := store.CountFluxMonitorRoundStats()
	require.NoError(t, err)
	require.Equal(t, 10, count)

	roundID, err := store.MostRecentFluxMonitorRoundID(cltest.NewAddress())
	require.Error(t, err)
	require.Equal(t, uint32(0), roundID)

	roundID, err = store.MostRecentFluxMonitorRoundID(address)
	require.NoError(t, err)
	require.Equal(t, uint32(9), roundID)
}

func TestStore_DeleteFluxMonitorRoundsBackThrough(t *testing.T) {
	t.Parallel()

	corestore, cleanup := cltest.NewStore(t)
	defer cleanup()

	jobORM := new(jobmocks.ORM)
	pipelineORM := new(pipelinemocks.ORM)
	store := fluxmonitorv2.NewStore(corestore.DB, corestore, jobORM, pipelineORM)

	address := cltest.NewAddress()

	for round := uint32(0); round < 10; round++ {
		_, err := store.FindOrCreateFluxMonitorRoundStats(address, round)
		require.NoError(t, err)
	}

	count, err := store.CountFluxMonitorRoundStats()
	require.NoError(t, err)
	require.Equal(t, 10, count)

	err = store.DeleteFluxMonitorRoundsBackThrough(cltest.NewAddress(), 5)
	require.NoError(t, err)

	count, err = store.CountFluxMonitorRoundStats()
	require.NoError(t, err)
	require.Equal(t, 10, count)

	err = store.DeleteFluxMonitorRoundsBackThrough(address, 5)
	require.NoError(t, err)

	count, err = store.CountFluxMonitorRoundStats()
	require.NoError(t, err)
	require.Equal(t, 5, count)
}

func TestStore_UpdateFluxMonitorRoundStats(t *testing.T) {
	t.Parallel()

	corestore, cleanup := cltest.NewStore(t)
	defer cleanup()

	// Instantiate a real pipeline ORM because we need to create a pipeline run
	// for the foreign key constraint of the stats record
	eventBroadcaster := postgres.NewEventBroadcaster(
		corestore.Config.DatabaseURL(),
		corestore.Config.DatabaseListenerMinReconnectInterval(),
		corestore.Config.DatabaseListenerMaxReconnectDuration(),
	)
	pipelineORM := pipeline.NewORM(corestore.ORM.DB, corestore.Config, eventBroadcaster)
	// Instantiate a real job ORM because we need to create a job to satisfy
	// a check in pipeline.CreateRun
	jobORM := job.NewORM(corestore.ORM.DB, corestore.Config, pipelineORM, eventBroadcaster, &postgres.NullAdvisoryLocker{})
	store := fluxmonitorv2.NewStore(corestore.DB, corestore, jobORM, pipelineORM)

	address := cltest.NewAddress()
	var roundID uint32 = 1

	j := makeJob(t)
	err := jobORM.CreateJob(context.Background(), j, *pipeline.NewTaskDAG())
	require.NoError(t, err)

	for expectedCount := uint64(1); expectedCount < 4; expectedCount++ {
		runID, err := pipelineORM.CreateRun(context.Background(), j.ID, map[string]interface{}{})
		require.NoError(t, err)

		err = store.UpdateFluxMonitorRoundStats(address, roundID, runID)
		require.NoError(t, err)

		stats, err := store.FindOrCreateFluxMonitorRoundStats(address, roundID)
		require.NoError(t, err)
		require.Equal(t, expectedCount, stats.NumSubmissions)
		require.True(t, stats.PipelineRunID.Valid)
		require.Equal(t, runID, stats.PipelineRunID.Int64)
	}
}

func TestStore_FindPipelineRun(t *testing.T) {
	t.Parallel()

	corestore, cleanup := cltest.NewStore(t)
	defer cleanup()

	jobORM := new(jobmocks.ORM)
	pipelineORM := new(pipelinemocks.ORM)
	store := fluxmonitorv2.NewStore(corestore.DB, corestore, jobORM, pipelineORM)

	runID := int64(1)
	run := pipeline.Run{}

	pipelineORM.On("FindRun", runID).Return(run, nil).Once()

	result, err := store.FindPipelineRun(runID)
	require.NoError(t, err)
	require.Equal(t, run, result)

	pipelineORM.AssertExpectations(t)
}

func makeJob(t *testing.T) *job.SpecDB {
	t.Helper()

	return &job.SpecDB{
		IDEmbed:       job.IDEmbed{ID: 1},
		Type:          "fluxmonitor",
		SchemaVersion: 1,
		Pipeline:      *pipeline.NewTaskDAG(),
		FluxMonitorSpec: &job.FluxMonitorSpec{
			IDEmbed:           job.IDEmbed{ID: 2},
			ContractAddress:   cltest.NewEIP55Address(),
			Precision:         2,
			Threshold:         0.5,
			PollTimerPeriod:   1 * time.Second,
			PollTimerDisabled: false,
			IdleTimerPeriod:   1 * time.Minute,
			IdleTimerDisabled: false,
			CreatedAt:         time.Now(),
			UpdatedAt:         time.Now(),
		},
	}
}
