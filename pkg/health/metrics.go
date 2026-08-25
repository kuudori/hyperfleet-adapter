package health

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsServer provides HTTP metrics endpoint for Prometheus.
type MetricsServer struct {
	server    *http.Server
	buildInfo *prometheus.GaugeVec
	upGauge   prometheus.Gauge
	port      string
}

// MetricsConfig holds configuration for metrics registration.
type MetricsConfig struct {
	Component string
	Version   string
	Commit    string
}

// NewMetricsServer creates a new metrics server with required HyperFleet metrics.
func NewMetricsServer(port string, cfg MetricsConfig) *MetricsServer {
	// Create build_info metric per HyperFleet metrics standard
	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "hyperfleet_adapter_build_info",
			Help: "Build information for the adapter",
		},
		[]string{"component", "version", "commit"},
	)

	// Create up metric per HyperFleet metrics standard
	upGauge := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "hyperfleet_adapter_up",
			Help: "Whether the adapter is up and running",
			ConstLabels: prometheus.Labels{
				"component": cfg.Component,
				"version":   cfg.Version,
			},
		},
	)

	// Register metrics
	prometheus.MustRegister(buildInfo)
	prometheus.MustRegister(upGauge)

	// Set build_info to 1 (this is an info metric)
	buildInfo.WithLabelValues(cfg.Component, cfg.Version, cfg.Commit).Set(1)

	// Set up to 1 (adapter is running)
	upGauge.Set(1)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	return &MetricsServer{
		port:      port,
		upGauge:   upGauge,
		buildInfo: buildInfo,
		server: &http.Server{
			Addr:              ":" + port,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
}

// Start starts the metrics server in a goroutine.
func (s *MetricsServer) Start(ctx context.Context) error {
	slog.InfoContext(ctx, "starting metrics server", "port", s.port)

	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.ErrorContext(ctx, "metrics server error", "error", err)
		}
	}()

	return nil
}

// MarkShuttingDown sets hyperfleet_adapter_up to 0 without stopping the metrics HTTP server.
func (s *MetricsServer) MarkShuttingDown() {
	s.upGauge.Set(0)
}

// Shutdown gracefully shuts down the metrics server.
func (s *MetricsServer) Shutdown(ctx context.Context) error {
	slog.InfoContext(ctx, "shutting down metrics server...")
	s.MarkShuttingDown()
	return s.server.Shutdown(ctx)
}
