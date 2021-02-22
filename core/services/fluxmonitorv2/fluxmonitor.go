package fluxmonitorv2

import (
	"github.com/smartcontractkit/chainlink/core/internal/gethwrappers/generated/flux_aggregator_wrapper"
	"github.com/smartcontractkit/chainlink/core/services/eth/contracts"
)

var fluxAggregatorABI = contracts.MustGetABI(flux_aggregator_wrapper.FluxAggregatorABI)
