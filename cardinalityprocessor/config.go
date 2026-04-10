// Package cardinalityprocessor is documented in processor.go.
package cardinalityprocessor

import "fmt"

// Config defines the user-facing configuration for the cardinality_guardian
// processor. Every field maps directly to a key in the OpenTelemetry Collector
// YAML configuration file under the processor's stanza, for example:
//
//	processors:
//	  cardinality_guardian:
//	    max_cardinality_delta_per_epoch: 500
//	    epoch_duration_seconds: 300
//	    never_drop_labels: [region, http.status_code]
//	    tag_only: false
//	    estimated_cost_per_metric_month: 0.05
type Config struct {
	// MaxCardinalityDeltaPerEpoch is the maximum number of new unique label
	// values that are allowed for a single metric+label-key combination within
	// one epoch. Once this threshold is exceeded, additional unique values are
	// either dropped (TagOnly: false) or tagged for cold-storage routing
	// (TagOnly: true).
	//
	// The processor measures cardinality growth using a HyperLogLog sketch and
	// compares the current epoch's estimate against the previous epoch's
	// estimate. Only the *delta* (new unique values seen this epoch) is checked,
	// not the absolute cardinality. This prevents the processor from penalizing
	// stable high-cardinality metrics that have already reached a steady state.
	//
	// Must be greater than 0.
	MaxCardinalityDeltaPerEpoch int `mapstructure:"max_cardinality_delta_per_epoch"`

	// EpochDurationSeconds controls how often the sliding cardinality window
	// advances. At the end of each epoch the processor promotes the current
	// HyperLogLog sketch to "previous" and starts a fresh sketch for the new
	// epoch. The delta check then measures growth relative to the boundary of
	// the last epoch, not the lifetime of the processor.
	//
	// Shorter epochs are more sensitive to sudden cardinality explosions but
	// may produce noisier decisions for metrics with naturally bursty label
	// spaces. Longer epochs smooth out transient bursts at the cost of
	// slower reaction time.
	//
	// Must be at least 10 seconds to avoid runaway ticker behavior.
	EpochDurationSeconds int `mapstructure:"epoch_duration_seconds"`

	// NeverDropLabels is the list of label keys that the processor will never
	// strip or tag, regardless of how high their cardinality grows. Use this
	// for labels whose values are essential for query correctness, such as
	// "region", "http.status_code", or "service.name".
	//
	// The lookup is O(1) via a pre-built map; the slice is only read at
	// construction time and never accessed in the hot path.
	NeverDropLabels []string `mapstructure:"never_drop_labels"`

	// TagOnly switches the processor from hard-drop mode to dual-route tagging
	// mode. When false (the default), high-cardinality attributes are silently
	// removed from the data point, keeping expensive time-series databases clean.
	// When true, the attribute is preserved and a boolean attribute
	// "otel.metric.overflow: true" is injected into the same data point.
	//
	// The injected tag is designed to be consumed by a downstream OTel routing
	// processor: metrics with the tag can be forwarded to cheap object storage
	// (S3, GCS, etc.) while clean metrics continue to flow into Prometheus or
	// Datadog. This makes the cardinality killer non-destructive and reversible,
	// which is valuable in regulated environments or during initial rollout.
	TagOnly bool `mapstructure:"tag_only"`

	// EstimatedCostPerMetricMonth is the dollar value assigned to each unique
	// time series that the processor prevents from entering a paid TSDB. It is
	// used solely to populate the "estimated_savings_dollars_total" OTel counter
	// for cost-visibility dashboards; it has no effect on the processor's
	// cardinality-enforcement logic.
	//
	// A reasonable starting point is $0.05/metric/month, which corresponds
	// roughly to the per-series pricing of managed Prometheus offerings.
	// Set to 0.0 to disable cost tracking without affecting enforcement.
	//
	// Must be ≥ 0.
	EstimatedCostPerMetricMonth float64 `mapstructure:"estimated_cost_per_metric_month"`

	// TopOffendersCount is the number of highest-delta (metric, label) pairs
	// to report via the "processor_top_offenders" gauge. The snapshot is
	// computed once per epoch rotation, so it adds no hot-path cost.
	//
	// Set to 0 to disable the Top-N gauge entirely.
	// Must be ≥ 0.
	TopOffendersCount int `mapstructure:"top_offenders_count"`
}

// Validate checks that all required Config fields are within their acceptable
// ranges and returns a descriptive error if any constraint is violated. The
// OTel Collector framework calls Validate automatically during pipeline
// construction; a non-nil return value prevents the pipeline from starting.
func (c *Config) Validate() error {
	if c.MaxCardinalityDeltaPerEpoch <= 0 {
		return fmt.Errorf("max_cardinality_delta_per_epoch must be greater than 0")
	}
	if c.EpochDurationSeconds < 10 {
		return fmt.Errorf("epoch_duration_seconds must be at least 10")
	}
	if c.EstimatedCostPerMetricMonth < 0 {
		return fmt.Errorf("estimated_cost_per_metric_month cannot be negative")
	}
	if c.TopOffendersCount < 0 {
		return fmt.Errorf("top_offenders_count cannot be negative")
	}
	return nil
}
