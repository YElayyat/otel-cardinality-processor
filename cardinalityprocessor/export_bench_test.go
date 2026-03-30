package cardinalityprocessor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/processor/processortest"
)

func TestNewCardinalityProcessorForTest(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	next := new(consumertest.MetricsSink)
	set := processortest.NewNopSettings(component.MustNewType("cardinality_guardian"))

	proc, err := NewCardinalityProcessorForTest(context.Background(), cfg, set, next)
	require.NoError(t, err)
	require.NotNil(t, proc)
}
