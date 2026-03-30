package cardinalityprocessor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *Config
		expectedErr string
	}{
		{
			name: "valid config",
			cfg: &Config{
				MaxCardinalityDeltaPerEpoch: 50,
				EpochDurationSeconds:        300,
				EstimatedCostPerMetricMonth: 0.10,
			},
			expectedErr: "",
		},
		{
			name: "invalid max_cardinality_delta_per_epoch",
			cfg: &Config{
				MaxCardinalityDeltaPerEpoch: 0,
				EpochDurationSeconds:        300,
			},
			expectedErr: "max_cardinality_delta_per_epoch must be greater than 0",
		},
		{
			name: "invalid epoch_duration_seconds",
			cfg: &Config{
				MaxCardinalityDeltaPerEpoch: 50,
				EpochDurationSeconds:        5,
			},
			expectedErr: "epoch_duration_seconds must be at least 10",
		},
		{
			name: "invalid estimated_cost_per_metric_month",
			cfg: &Config{
				MaxCardinalityDeltaPerEpoch: 50,
				EpochDurationSeconds:        300,
				EstimatedCostPerMetricMonth: -0.5,
			},
			expectedErr: "estimated_cost_per_metric_month cannot be negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.expectedErr == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedErr)
			}
		})
	}
}
