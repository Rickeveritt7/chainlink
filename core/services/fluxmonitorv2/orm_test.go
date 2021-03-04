package fluxmonitorv2_test

import (
	"context"
	"testing"
	"time"

	"github.com/smartcontractkit/chainlink/core/internal/cltest"
	"github.com/smartcontractkit/chainlink/core/services/fluxmonitorv2"
	"github.com/smartcontractkit/chainlink/core/services/job"
	"github.com/smartcontractkit/chainlink/core/services/pipeline"
	"github.com/smartcontractkit/chainlink/core/services/postgres"
	"github.com/stretchr/testify/require"
)

func TestORM_FindOrCreateFluxMonitorRoundStats(t *testing.T) {
	t.Parallel()

	corestore, cleanup := cltest.NewStore(t)
	t.Cleanup(cleanup)

	orm := fluxmonitorv2.NewORM(corestore.DB)

	address := cltest.NewAddress()
	var roundID uint32 = 1

	stats, err := orm.FindOrCreateFluxMonitorRoundStats(address, roundID)
	require.NoError(t, err)
	require.Equal(t, roundID, stats.RoundID)
	require.Equal(t, address, stats.Aggregator)

	count, err := orm.CountFluxMonitorRoundStats()
	require.NoError(t, err)
	require.Equal(t, 1, count)

	stats, err = orm.FindOrCreateFluxMonitorRoundStats(address, roundID)
	require.NoError(t, err)
	require.Equal(t, roundID, stats.RoundID)
	require.Equal(t, address, stats.Aggregator)

	count, err = orm.CountFluxMonitorRoundStats()
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestORM_MostRecentFluxMonitorRoundID(t *testing.T) {
	t.Parallel()

	corestore, cleanup := cltest.NewStore(t)
	t.Cleanup(cleanup)

	orm := fluxmonitorv2.NewORM(corestore.DB)

	address := cltest.NewAddress()

	for round := uint32(0); round < 10; round++ {
		_, err := orm.FindOrCreateFluxMonitorRoundStats(address, round)
		require.NoError(t, err)
	}

	count, err := orm.CountFluxMonitorRoundStats()
	require.NoError(t, err)
	require.Equal(t, 10, count)

	roundID, err := orm.MostRecentFluxMonitorRoundID(cltest.NewAddress())
	require.Error(t, err)
	require.Equal(t, uint32(0), roundID)

	roundID, err = orm.MostRecentFluxMonitorRoundID(address)
	require.NoError(t, err)
	require.Equal(t, uint32(9), roundID)
}

func TestORM_DeleteFluxMonitorRoundsBackThrough(t *testing.T) {
	t.Parallel()

	corestore, cleanup := cltest.NewStore(t)
	t.Cleanup(cleanup)

	orm := fluxmonitorv2.NewORM(corestore.DB)

	address := cltest.NewAddress()

	for round := uint32(0); round < 10; round++ {
		_, err := orm.FindOrCreateFluxMonitorRoundStats(address, round)
		require.NoError(t, err)
	}

	count, err := orm.CountFluxMonitorRoundStats()
	require.NoError(t, err)
	require.Equal(t, 10, count)

	err = orm.DeleteFluxMonitorRoundsBackThrough(cltest.NewAddress(), 5)
	require.NoError(t, err)

	count, err = orm.CountFluxMonitorRoundStats()
	require.NoError(t, err)
	require.Equal(t, 10, count)

	err = orm.DeleteFluxMonitorRoundsBackThrough(address, 5)
	require.NoError(t, err)

	count, err = orm.CountFluxMonitorRoundStats()
	require.NoError(t, err)
	require.Equal(t, 5, count)
}

func TestORM_UpdateFluxMonitorRoundStats(t *testing.T) {
	t.Parallel()

	corestore, cleanup := cltest.NewStore(t)
	t.Cleanup(cleanup)

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
	orm := fluxmonitorv2.NewORM(corestore.DB)

	address := cltest.NewAddress()
	var roundID uint32 = 1

	j := makeJob(t)
	err := jobORM.CreateJob(context.Background(), j, *pipeline.NewTaskDAG())
	require.NoError(t, err)

	for expectedCount := uint64(1); expectedCount < 4; expectedCount++ {
		runID, err := pipelineORM.CreateRun(context.Background(), j.ID, map[string]interface{}{})
		require.NoError(t, err)

		err = orm.UpdateFluxMonitorRoundStats(address, roundID, runID)
		require.NoError(t, err)

		stats, err := orm.FindOrCreateFluxMonitorRoundStats(address, roundID)
		require.NoError(t, err)
		require.Equal(t, expectedCount, stats.NumSubmissions)
		require.True(t, stats.PipelineRunID.Valid)
		require.Equal(t, runID, stats.PipelineRunID.Int64)
	}
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
