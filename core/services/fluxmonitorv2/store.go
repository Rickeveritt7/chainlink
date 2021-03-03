package fluxmonitorv2

import (
	"context"
	"encoding/hex"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/pkg/errors"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services/job"
	"github.com/smartcontractkit/chainlink/core/services/pipeline"
	corestore "github.com/smartcontractkit/chainlink/core/store"
	"gorm.io/gorm"
)

//go:generate mockery --name Store --output ./mocks/ --case=underscore

type Store interface {
	RecordError(jobID int32, description string)
	KeyStoreAccounts() []accounts.Account
	MostRecentFluxMonitorRoundID(aggregator common.Address) (uint32, error)
	DeleteFluxMonitorRoundsBackThrough(aggregator common.Address, roundID uint32) error
	FindOrCreateFluxMonitorRoundStats(aggregator common.Address, roundID uint32) (FluxMonitorRoundStatsV2, error)
	UpdateFluxMonitorRoundStats(aggregator common.Address, roundID uint32, runID int64) error
	FindPipelineRun(runID int64) (pipeline.Run, error)
	GetRoundRobinAddress() (common.Address, error)
}

type store struct {
	db          *gorm.DB
	cstore      *corestore.Store
	jobORM      job.ORM
	pipelineORM pipeline.ORM
}

func NewStore(
	db *gorm.DB,
	cstore *corestore.Store,
	jobORM job.ORM,
	pipelineORM pipeline.ORM,
) *store {
	return &store{
		db,
		cstore,
		jobORM,
		pipelineORM,
	}
}

// RecordError records an job error in the DB. wraps the jobORM RecordError
// method, supplying an empty context.
func (s *store) RecordError(jobID int32, description string) {
	s.jobORM.RecordError(context.Background(), jobID, description)
}

// KeyStoreAccounts gets the node's keys
func (s *store) KeyStoreAccounts() []accounts.Account {
	return s.cstore.KeyStore.Accounts()
}

// MostRecentFluxMonitorRoundID finds roundID of the most recent round that the
// provided oracle address submitted to
func (s *store) MostRecentFluxMonitorRoundID(aggregator common.Address) (uint32, error) {
	var stats FluxMonitorRoundStatsV2
	err := s.db.
		Order("round_id DESC").
		First(&stats, "aggregator = ?", aggregator).
		Error
	if err != nil {
		return 0, err
	}

	return stats.RoundID, nil
}

// DeleteFluxMonitorRoundsBackThrough deletes all the RoundStat records for a
// given oracle address starting from the most recent round back through the
// given round
func (s *store) DeleteFluxMonitorRoundsBackThrough(aggregator common.Address, roundID uint32) error {
	return s.db.Exec(`
        DELETE FROM flux_monitor_round_stats_v2
        WHERE aggregator = ?
          AND round_id >= ?
    `, aggregator, roundID).Error
}

// FindOrCreateFluxMonitorRoundStats find the round stats record for a given
// oracle on a given round, or creates it if no record exists
func (s *store) FindOrCreateFluxMonitorRoundStats(aggregator common.Address, roundID uint32) (FluxMonitorRoundStatsV2, error) {
	var stats FluxMonitorRoundStatsV2
	err := s.db.FirstOrCreate(&stats,
		FluxMonitorRoundStatsV2{Aggregator: aggregator, RoundID: roundID},
	).Error

	return stats, err
}

// UpdateFluxMonitorRoundStats trys to create a RoundStat record for the given oracle
// at the given round. If one already exists, it increments the num_submissions column.
func (s *store) UpdateFluxMonitorRoundStats(aggregator common.Address, roundID uint32, runID int64) error {
	return s.db.Exec(`
        INSERT INTO flux_monitor_round_stats_v2 (
            aggregator, round_id, pipeline_run_id, num_new_round_logs, num_submissions
        ) VALUES (
            ?, ?, ?, 0, 1
        ) ON CONFLICT (aggregator, round_id)
        DO UPDATE SET
					num_submissions = flux_monitor_round_stats_v2.num_submissions + 1,
					pipeline_run_id = EXCLUDED.pipeline_run_id
    `, aggregator, roundID, runID).Error
}

// CountFluxMonitorRoundStats counts the total number of records
func (s *store) CountFluxMonitorRoundStats() (int, error) {
	var count int64
	err := s.db.Table("flux_monitor_round_stats_v2").Count(&count).Error

	return int(count), err
}

// FindPipelineRun retrieves a pipeline.Run by id.
func (s *store) FindPipelineRun(runID int64) (pipeline.Run, error) {
	return s.pipelineORM.FindRun(runID)
}

// CreateEthTransaction creates an ethereum transaction for the BPTXM to pick up
func (s *store) CreateEthTransaction(
	fromAddress common.Address,
	toAddress common.Address,
	payload []byte,
	value *big.Int,
	gasLimit uint64,
) error {
	dbtx := s.db.Exec(`
INSERT INTO eth_txes (from_address, to_address, encoded_payload, value, gas_limit, state, created_at)
SELECT $1,$2,$3,$4,$5,'unstarted',NOW()
WHERE NOT EXISTS (
    SELECT 1 FROM eth_tx_attempts
	JOIN eth_txes ON eth_txes.id = eth_tx_attempts.eth_tx_id
	WHERE eth_txes.from_address = $1
		AND eth_txes.state = 'unconfirmed'
		AND eth_tx_attempts.state = 'insufficient_eth'
);
`, fromAddress, toAddress, payload, value, gasLimit)
	if dbtx.Error != nil {
		return errors.Wrap(dbtx.Error, "failed to insert eth_tx")
	}
	if dbtx.RowsAffected == 0 {
		// Unsure why this would be an wallet out of eth error
		err := errors.Errorf("Skipped OCR transmission because wallet is out of eth: %s", fromAddress.Hex())
		logger.Warnw(err.Error(),
			"fromAddress", fromAddress,
			"toAddress", toAddress,
			"payload", "0x"+hex.EncodeToString(payload),
			"value", value,
			"gasLimit", gasLimit,
		)

		return err
	}

	return nil
}

// GetRoundRobinAddress queries the database for the address of a random
// ethereum key derived from the id.
func (s *store) GetRoundRobinAddress() (common.Address, error) {
	return s.cstore.GetRoundRobinAddress()
}
