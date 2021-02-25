package keeper

import (
	"github.com/pelletier/go-toml"
	"github.com/pkg/errors"
	"github.com/smartcontractkit/chainlink/core/services/job"
	"github.com/smartcontractkit/chainlink/core/services/pipeline"
)

func ValidatedKeeperSpec(tomlString string) (job.SpecDB, error) {
	var specDB = job.SpecDB{
		Pipeline: *pipeline.NewTaskDAG(),
	}
	var spec job.KeeperSpec
	tree, err := toml.Load(tomlString)
	if err != nil {
		return specDB, err
	}
	err = tree.Unmarshal(&specDB)
	if err != nil {
		return specDB, err
	}
	err = tree.Unmarshal(&spec)
	if err != nil {
		return specDB, err
	}
	specDB.KeeperSpec = &spec

	if specDB.Type != job.Keeper {
		return specDB, errors.Errorf("unsupported type %s", specDB.Type)
	}
	if specDB.SchemaVersion != uint32(1) {
		return specDB, errors.Errorf("the only supported schema version is currently 1, got %v", specDB.SchemaVersion)
	}
	return specDB, nil
}
