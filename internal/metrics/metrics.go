package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	transitionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llm_operator_transition_duration_seconds",
			Help:    "Duration of model transitions.",
			Buckets: []float64{60, 300, 600, 1800, 3600},
		},
		[]string{"model_name"},
	)

	transitionFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_operator_transition_failures_total",
			Help: "Total number of failed model transitions.",
		},
		[]string{"model_name", "reason"},
	)

	modelSwitches = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_operator_model_switches_total",
			Help: "Total number of model switches.",
		},
		[]string{"from_model", "to_model", "backend_type"},
	)
)

func Register() {
	prometheus.MustRegister(transitionDuration)
	prometheus.MustRegister(transitionFailures)
	prometheus.MustRegister(modelSwitches)
}

func RecordTransitionDuration(d time.Duration, modelName string) {
	transitionDuration.WithLabelValues(modelName).Observe(d.Seconds())
}

func RecordTransitionFailure(modelName, reason string) {
	transitionFailures.WithLabelValues(modelName, reason).Inc()
}

func RecordModelSwitch(fromModel, toModel, backendType string) {
	modelSwitches.WithLabelValues(fromModel, toModel, backendType).Inc()
}
