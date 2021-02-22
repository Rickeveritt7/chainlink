package fluxmonitorv2

import (
	"context"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/smartcontractkit/chainlink/core/services/job"
	"github.com/smartcontractkit/chainlink/core/services/pipeline"
	corestore "github.com/smartcontractkit/chainlink/core/store"
	"github.com/smartcontractkit/chainlink/core/store/models"
	"gorm.io/gorm"
)

type Store interface {
	RecordError(jobID int32, description string)
	KeyStoreAccounts() []accounts.Account
	MostRecentFluxMonitorRoundID(aggregator common.Address) (uint32, error)
	DeleteFluxMonitorRoundsBackThrough(aggregator common.Address, roundID uint32) error
	FindOrCreateFluxMonitorRoundStats(aggregator common.Address, roundID uint32) (models.FluxMonitorRoundStats, error)
	FindPipelineRun()
}

type store struct {
	db          *gorm.DB
	cstore      corestore.Store
	jobORM      job.ORM
	pipelineORM pipeline.ORM
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
        DELETE FROM flux_monitor_round_stats
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

// FindPipelineRun retrieves a pipeline.Run by id.
func (s *store) FindPipelineRun(runID int64) (pipeline.Run, error) {
	return s.pipelineORM.FindRun(runID)
}

// db wraps the the core store and job orm.
//
// TODO - Reimplement the store methods used to remove the dependency on the
// store package
type Database struct {
	store  *corestore.Store
	jobORM job.ORM
}

// newDatabase constructs a new database
func NewDatabase(store *corestore.Store, jobORM job.ORM) *Database {
	return &Database{
		store:  store,
		jobORM: jobORM,
	}
}

// RecordError wraps the job ORM record error message, supplying an empty
// context.
func (db *Database) RecordError(jobID int32, description string) {
	db.jobORM.RecordError(context.Background(), jobID, description)
}

// KeyStoreAccounts gets the node's keys
func (db *Database) KeyStoreAccounts() []accounts.Account {
	return db.store.KeyStore.Accounts()
}

// MostRecentFluxMonitorRoundID fetches the most recent Flux Monitor Round ID
func (db *Database) MostRecentFluxMonitorRoundID(aggregator common.Address) (uint32, error) {
	return db.store.MostRecentFluxMonitorRoundID(aggregator)
}

// DeleteFluxMonitorRoundsBackThrough deletes all the RoundStat records for a
// given oracle address starting from the most recent round back through the
// given round
func (db *Database) DeleteFluxMonitorRoundsBackThrough(aggregator common.Address, roundID uint32) error {
	return db.store.DeleteFluxMonitorRoundsBackThrough(aggregator, roundID)
}

func (db *Database) FindOrCreateFluxMonitorRoundStats(aggregator common.Address, roundID uint32) (models.FluxMonitorRoundStats, error) {
	return db.store.FindOrCreateFluxMonitorRoundStats(aggregator, roundID)
}
