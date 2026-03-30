package cardinalityprocessor

import (
	"context"

	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
)

// NewCardinalityProcessorForTest is a public constructor that allows external
// packages (e.g. test/benchmark/) to instantiate the processor without going
// through the factory. It wraps the internal newCardinalityProcessor.
//
// This function is NOT part of the public API and should not be relied upon
// by production consumers. Use NewFactory() instead.
func NewCardinalityProcessorForTest(ctx context.Context, cfg *Config, set processor.Settings, next consumer.Metrics) (processor.Metrics, error) {
	return newCardinalityProcessor(ctx, cfg, set, next)
}
