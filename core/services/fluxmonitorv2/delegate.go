package fluxmonitorv2

import (
	"github.com/pkg/errors"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services/eth"
	"github.com/smartcontractkit/chainlink/core/services/job"
	"github.com/smartcontractkit/chainlink/core/services/log"
	"github.com/smartcontractkit/chainlink/core/services/pipeline"
	corestore "github.com/smartcontractkit/chainlink/core/store"
	"gorm.io/gorm"
)

type Delegate struct {
	db             *gorm.DB
	store          *corestore.Store
	jobORM         job.ORM
	pipelineORM    pipeline.ORM
	pipelineRunner pipeline.Runner
	ethClient      eth.Client
	logBroadcaster log.Broadcaster
	cfg            Config
}

// NewDelegate constructs a new delegate
func NewDelegate(
	store *corestore.Store,
	jobORM job.ORM,
	pipelineORM pipeline.ORM,
	pipelineRunner pipeline.Runner,
	db *gorm.DB,
	ethClient eth.Client,
	logBroadcaster log.Broadcaster,
	cfg Config,
) *Delegate {
	return &Delegate{
		db,
		store,
		jobORM,
		pipelineORM,
		pipelineRunner,
		ethClient,
		logBroadcaster,
		cfg,
	}
}

// JobType implements the job.Delegate interface
func (d *Delegate) JobType() job.Type {
	return job.FluxMonitor
}

// ServicesForSpec returns the flux monitor service for the job spec
func (d *Delegate) ServicesForSpec(spec job.SpecDB) (services []job.Service, err error) {
	if spec.FluxMonitorSpec == nil {
		return nil, errors.Errorf("Delegate expects a *job.FluxMonitorSpec to be present, got %v", spec)
	}

	factory := fluxMonitorFactory{
		orm:            NewORM(d.store.DB),
		jobORM:         d.jobORM,
		pipelineORM:    d.pipelineORM,
		keyStore:       NewKeyStore(d.store),
		ethClient:      d.store.EthClient,
		logBroadcaster: d.logBroadcaster,
	}

	fm, err := factory.New(
		Specification{
			ID:                spec.GetID(),
			JobID:             spec.FluxMonitorSpec.ID,
			ContractAddress:   spec.FluxMonitorSpec.ContractAddress.Address(),
			Precision:         spec.FluxMonitorSpec.Precision,
			Threshold:         float64(spec.FluxMonitorSpec.Threshold),
			AbsoluteThreshold: float64(spec.FluxMonitorSpec.AbsoluteThreshold),
			PollTimerPeriod:   spec.FluxMonitorSpec.PollTimerPeriod,
			PollTimerDisabled: spec.FluxMonitorSpec.PollTimerDisabled,
			IdleTimerPeriod:   spec.FluxMonitorSpec.IdleTimerPeriod,
			IdleTimerDisabled: spec.FluxMonitorSpec.IdleTimerDisabled,
			MinJobPayment:     spec.FluxMonitorSpec.MinPayment,
		},
		PipelineRun{
			runner: d.pipelineRunner,
			spec:   *spec.PipelineSpec,
			jobID:  spec.ID,
			logger: *logger.Default,
		},
		d.cfg,
	)
	if err != nil {
		return nil, err
	}

	return []job.Service{fm}, nil
}
