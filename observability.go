package silgotel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	pyroscope "github.com/grafana/pyroscope-go"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	otelMetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	otelTrace "go.opentelemetry.io/otel/trace"
)

type ctxKey string

var loggerKey ctxKey = "LoggingMiddlewareKey" //nolint: gochecknoglobals

//nolint:gochecknoglobals
var headers = map[string]string{
	"content-type": "application/json",
}

func (c *Client) setupOtelSDK(ctx context.Context) (func(context.Context) error, error) {
	var shutdownFuncs []func(context.Context) error

	shutdown := func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}

		shutdownFuncs = nil

		return err
	}

	res, err := c.newResource(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating resource: %w", err)
	}

	otel.SetTextMapPropagator(newPropagator())

	tracerProvider, err := c.newTracerProvider(ctx, res)
	if err != nil {
		return nil, errors.Join(err, shutdown(ctx))
	}

	shutdownFuncs = append(shutdownFuncs, tracerProvider.Shutdown)
	otel.SetTracerProvider(tracerProvider)

	meterProvider, err := c.newMeterProvider(ctx, res)
	if err != nil {
		return nil, errors.Join(err, shutdown(ctx))
	}

	shutdownFuncs = append(shutdownFuncs, meterProvider.Shutdown)
	otel.SetMeterProvider(meterProvider)

	loggerProvider, err := c.newLoggerProvider(ctx, res)
	if err != nil {
		return nil, errors.Join(err, shutdown(ctx))
	}

	shutdownFuncs = append(shutdownFuncs, loggerProvider.Shutdown)
	global.SetLoggerProvider(loggerProvider)

	return shutdown, nil
}

func (c *Client) newResource(_ context.Context) (*resource.Resource, error) {
	return resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String(c.ServiceName),
			semconv.ServiceVersion(c.Version),
			semconv.DeploymentEnvironmentName(c.Environment),
		),
	)
}

//nolint:ireturn
func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

func (c *Client) newTracerProvider(ctx context.Context, res *resource.Resource) (*trace.TracerProvider, error) {
	exporter, err := otlptrace.New(
		ctx,
		otlptracehttp.NewClient(
			otlptracehttp.WithEndpointURL(fmt.Sprintf("%s/v1/traces", c.OTLPBaseURL)),
			otlptracehttp.WithHeaders(headers),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("creating trace exporter: %w", err)
	}

	return trace.NewTracerProvider(
		trace.WithBatcher(exporter,
			trace.WithMaxExportBatchSize(trace.DefaultMaxExportBatchSize),
			trace.WithBatchTimeout(trace.DefaultScheduleDelay),
			trace.WithExportTimeout(10*time.Second),
		),
		trace.WithResource(res),
	), nil
}

func (c *Client) newMeterProvider(ctx context.Context, res *resource.Resource) (*sdkmetric.MeterProvider, error) {
	exporter, err := otlpmetrichttp.New(
		ctx,
		otlpmetrichttp.WithEndpointURL(fmt.Sprintf("%s/v1/metrics", c.OTLPBaseURL)),
		otlpmetrichttp.WithHeaders(headers),
	)
	if err != nil {
		return nil, fmt.Errorf("creating metric exporter: %w", err)
	}

	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(exporter,
				sdkmetric.WithInterval(30*time.Second),
				sdkmetric.WithTimeout(10*time.Second),
			),
		),
		WithHTTPViews(),
	), nil
}

//nolint:godoclint,ireturn
func WithHTTPViews() sdkmetric.Option {
	return sdkmetric.WithView(
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name: "http.server.request_duration",
				Kind: sdkmetric.InstrumentKindHistogram,
			},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: []float64{
						0.005, 0.01, 0.025, 0.05, 0.1,
						0.25, 0.5, 1, 2.5, 5, 10,
					},
				},
				Unit: "s",
			},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name: "http.server.request.body.size",
				Kind: sdkmetric.InstrumentKindHistogram,
			},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: []float64{
						256, 512, 1024, 4096, 16384,
						65536, 262144, 1048576, 4194304,
					},
				},
				Unit: "By",
			},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name: "http.server.response.body.size",
				Kind: sdkmetric.InstrumentKindHistogram,
			},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: []float64{
						256, 512, 1024, 4096, 16384,
						65536, 262144, 1048576, 4194304,
					},
				},
				Unit: "By",
			},
		),
	)
}

func (c *Client) newLoggerProvider(ctx context.Context, res *resource.Resource) (*log.LoggerProvider, error) {
	exporter, err := otlploghttp.New(
		ctx,
		otlploghttp.WithEndpointURL(fmt.Sprintf("%s/v1/logs", c.OTLPBaseURL)),
		otlploghttp.WithHeaders(headers),
	)
	if err != nil {
		return nil, fmt.Errorf("creating log exporter: %w", err)
	}

	return log.NewLoggerProvider(
		log.WithResource(res),
		log.WithProcessor(log.NewBatchProcessor(exporter)),
	), nil
}

// Trace starts a new span and returns the updated context.
//
//nolint:ireturn
func Trace(ctx context.Context, packageName, spanName string) (context.Context, otelTrace.Span) {
	tracer := otel.Tracer(packageName)
	ctx, span := tracer.Start(ctx, spanName) //nolint:spancheck

	//nolint:spancheck
	return ctx, span
}

// RecordError sets the span status to error and records the error event.
func RecordError(span otelTrace.Span, err error) {
	if err == nil {
		return
	}

	span.SetStatus(codes.Error, err.Error())
	span.RecordError(err)
}

// NewLogger returns a reusable slog.Logger bridged to OTel. Cache the result.
func NewLogger(packageName string) *slog.Logger {
	return otelslog.NewLogger(packageName)
}

// Meter returns a named meter for recording metrics.
//
//nolint:ireturn
func Meter(serviceName string) otelMetric.Meter {
	return otel.Meter(serviceName)
}

// Gin HTTP middleware configuration
//
//nolint:ireturn
func mustInstrument[T any](v T, err error) T {
	if err != nil {
		panic(fmt.Sprintf("silgotel: failed to create metric instrument: %v", err))
	}

	return v
}

type httpMetrics struct {
	requestCounter  otelMetric.Int64Counter
	requestDuration otelMetric.Float64Histogram
	activeRequests  otelMetric.Int64UpDownCounter
	requestSize     otelMetric.Int64Histogram
	responseSize    otelMetric.Int64Histogram
	cpuUsage        otelMetric.Float64ObservableGauge
	cpuSecondsTotal otelMetric.Float64ObservableCounter
	memoryUsage     otelMetric.Int64ObservableGauge
	heapObjects     otelMetric.Int64ObservableGauge
	goroutineCount  otelMetric.Int64ObservableGauge
	gcPauseTime     otelMetric.Float64ObservableGauge
}

type cpuStats struct {
	timestamp time.Time
	userTime  float64
	sysTime   float64
}

func newHTTPMetrics(serviceName string) *httpMetrics {
	meter := Meter(serviceName)

	var (
		lastCPUStats  cpuStats
		cpuStatsMutex sync.Mutex
	)

	metrics := &httpMetrics{
		requestCounter: mustInstrument(meter.Int64Counter(
			"http.server.requests_total",
			otelMetric.WithDescription("Total number of HTTP requests processed"),
			otelMetric.WithUnit("1"),
		)),
		requestDuration: mustInstrument(meter.Float64Histogram(
			"http.server.request_duration",
			otelMetric.WithDescription("HTTP request duration in seconds"),
			otelMetric.WithUnit("s"),
		)),
		activeRequests: mustInstrument(meter.Int64UpDownCounter(
			"http.server.active_requests",
			otelMetric.WithDescription("Number of active HTTP server requests"),
			otelMetric.WithUnit("1"),
		)),
		requestSize: mustInstrument(meter.Int64Histogram(
			"http.server.request.body.size",
			otelMetric.WithDescription("Size of request body in bytes"),
			otelMetric.WithUnit("By"),
		)),
		responseSize: mustInstrument(meter.Int64Histogram(
			"http.server.response.body.size",
			otelMetric.WithDescription("Size of response body in bytes"),
			otelMetric.WithUnit("By"),
		)),
		cpuUsage: mustInstrument(meter.Float64ObservableGauge(
			"system.cpu.usage",
			otelMetric.WithDescription("Percentage of CPU used"),
			otelMetric.WithUnit("%"),
			otelMetric.WithFloat64Callback(func(ctx context.Context, fo otelMetric.Float64Observer) error {
				usage, err := calculateCPUUsage(&lastCPUStats, &cpuStatsMutex)
				if err != nil {
					slog.Error("error calculating CPU metrics", "err", err)

					return nil
				}

				fo.Observe(usage)

				return nil
			}),
		)),
		cpuSecondsTotal: mustInstrument(meter.Float64ObservableCounter(
			"system.cpu.total_seconds",
			otelMetric.WithDescription("Total CPU time spend"),
			otelMetric.WithUnit("s"),
			otelMetric.WithFloat64Callback(func(ctx context.Context, fo otelMetric.Float64Observer) error {
				var rusage syscall.Rusage

				err := syscall.Getrusage(syscall.RUSAGE_SELF, &rusage)
				if err != nil {
					slog.Error("error calculating CPU metrics", "err", err)

					return nil
				}

				userSec := float64(rusage.Utime.Sec) + float64(rusage.Utime.Usec)/1e6
				sysSec := float64(rusage.Stime.Sec) + float64(rusage.Stime.Usec)/1e6

				fo.Observe(userSec, otelMetric.WithAttributes(attribute.String("mode", "user")))
				fo.Observe(sysSec, otelMetric.WithAttributes(attribute.String("mode", "system")))

				return nil
			}),
		)),
		goroutineCount: mustInstrument(meter.Int64ObservableGauge(
			"system.cpu.goroutines_count",
			otelMetric.WithDescription("Number of active goroutines in the system"),
			otelMetric.WithUnit("1"),
			otelMetric.WithInt64Callback(func(_ context.Context, io otelMetric.Int64Observer) error {
				io.Observe(int64(runtime.NumGoroutine()))

				return nil
			}),
		)),
	}

	metrics.memoryUsage = mustInstrument(meter.Int64ObservableGauge(
		"system.memory.usage",
		otelMetric.WithDescription("Total system memory used"),
		otelMetric.WithUnit("By"),
	))
	metrics.heapObjects = mustInstrument(meter.Int64ObservableGauge(
		"system.memory.heap_objects",
		otelMetric.WithDescription("Number of heap objects in memory"),
		otelMetric.WithUnit("1"),
	))
	metrics.gcPauseTime = mustInstrument(meter.Float64ObservableGauge(
		"system.memory.gc_pause_time",
		otelMetric.WithDescription("Most recent GC pause duration"),
		otelMetric.WithUnit("s"),
	))

	_, err := meter.RegisterCallback(
		func(_ context.Context, o otelMetric.Observer) error {
			var m runtime.MemStats

			runtime.ReadMemStats(&m)

			o.ObserveInt64(metrics.memoryUsage,
				int64(m.Alloc), // nolint: gosec
				otelMetric.WithAttributes(attribute.String("type", "alloc")),
			)
			o.ObserveInt64(metrics.memoryUsage,
				int64(m.HeapAlloc), // nolint: gosec
				otelMetric.WithAttributes(attribute.String("type", "heap_alloc")),
			)
			o.ObserveInt64(metrics.memoryUsage,
				int64(m.HeapInuse), // nolint: gosec
				otelMetric.WithAttributes(attribute.String("type", "heap_inuse")),
			)
			o.ObserveInt64(metrics.memoryUsage,
				int64(m.HeapIdle), // nolint: gosec
				otelMetric.WithAttributes(attribute.String("type", "heap_idle")),
			)
			o.ObserveInt64(metrics.memoryUsage,
				int64(m.StackInuse), // nolint: gosec
				otelMetric.WithAttributes(attribute.String("type", "stack_inuse")),
			)
			o.ObserveInt64(metrics.memoryUsage,
				int64(m.Sys), // nolint: gosec
				otelMetric.WithAttributes(attribute.String("type", "sys")),
			)

			o.ObserveInt64(metrics.heapObjects, int64(m.HeapObjects)) // nolint: gosec

			if m.NumGC > 0 {
				idx := (m.NumGC + 255) % 256
				o.ObserveFloat64(metrics.gcPauseTime, float64(m.PauseNs[idx])/1e9)
			}

			return nil
		},
		metrics.memoryUsage,
		metrics.heapObjects,
		metrics.gcPauseTime,
	)
	if err != nil {
		panic(fmt.Sprintf("silgotel: failed to register memory metrics callback: %v", err))
	}

	return metrics
}

func calculateCPUUsage(stats *cpuStats, statsMutex *sync.Mutex) (float64, error) {
	var rusage syscall.Rusage

	err := syscall.Getrusage(syscall.RUSAGE_SELF, &rusage)
	if err != nil {
		return 0, err
	}

	currentUserTime := float64(rusage.Utime.Sec) + float64(rusage.Utime.Usec)/1e6
	currentSysTime := float64(rusage.Stime.Sec) + float64(rusage.Stime.Usec)/1e6
	now := time.Now()

	statsMutex.Lock()
	defer statsMutex.Unlock()

	if stats.timestamp.IsZero() {
		stats.timestamp = now
		stats.sysTime = currentSysTime
		stats.userTime = currentUserTime

		return 0, nil
	}

	elapsed := now.Sub(stats.timestamp).Seconds()
	if elapsed <= 0 {
		return 0, nil
	}

	delta := (currentUserTime - stats.userTime) + (currentSysTime - stats.sysTime)
	usage := (delta / elapsed) * 100

	stats.timestamp = now
	stats.userTime = currentUserTime
	stats.sysTime = currentSysTime

	maxCPU := float64(runtime.NumCPU()) * 100
	if usage > maxCPU {
		usage = maxCPU
	}

	return usage, nil
}

// RequestMetrics returns a standard net/http middleware that records HTTP server metrics.
// The serviceName should match the service name used in NewOtelSDK.
func RequestMetrics(serviceName string) func(http.Handler) http.Handler {
	m := newHTTPMetrics(serviceName)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			scheme := r.Header.Get("X-Forwarded-Proto")
			if scheme == "" {
				scheme = "http"
			}

			route := normalizedRoutePattern(r) // consumers can enrich this via context if needed

			baseAttrs := otelMetric.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.route", route),
				attribute.String("url.scheme", scheme),
			)

			m.activeRequests.Add(ctx, 1, baseAttrs)

			if r.ContentLength > 0 {
				m.requestSize.Record(ctx, r.ContentLength, baseAttrs)
			}

			rw := newResponseWriter(w)
			start := time.Now()

			next.ServeHTTP(rw, r)

			endAttrs := otelMetric.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.route", route),
				attribute.String("url.scheme", scheme),
				attribute.Int("http.status_code", rw.status),
			)

			durationSec := time.Since(start).Seconds()

			m.requestCounter.Add(ctx, 1, endAttrs)
			m.requestDuration.Record(ctx, durationSec, endAttrs)
			m.responseSize.Record(ctx, int64(rw.size), endAttrs)
			m.activeRequests.Add(ctx, -1, baseAttrs)
		})
	}
}

// responseWriter wraps http.ResponseWriter to capture status code and response size.
type responseWriter struct {
	http.ResponseWriter

	status int
	size   int
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w, status: http.StatusOK}
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n

	return n, err
}

func GinRequestMetrics(serviceName string) gin.HandlerFunc {
	middleware := RequestMetrics(serviceName)

	return func(c *gin.Context) {
		middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.Request = r
			c.Next()
		})).ServeHTTP(c.Writer, c.Request)
	}
}

// StartProfiler starts and returns a pyroscope profiler. Caller must explicitly
// call profiler.Stop() upon application exit.
func StartProfiler(endpoint, serviceName, environment string) (*pyroscope.Profiler, error) {
	profiler, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: serviceName,
		Logger:          pyroscope.StandardLogger,
		ServerAddress:   endpoint,
		Tags:            map[string]string{"environment": environment},
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileGoroutines,
			pyroscope.ProfileMutexCount,
			pyroscope.ProfileMutexDuration,
			pyroscope.ProfileBlockCount,
			pyroscope.ProfileBlockDuration,
		},
	})

	return profiler, err
}

// LoggingMiddleware returns a standard net/http middleware for logging.
//
//	r.Use(silotel.LoggingMiddleware("example"))
//
// Check LoggerFromContext for usage details.
func LoggingMiddleware(serviceName string) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger := slog.With(
				"request_id", uuid.New().String(),
				"method", r.Method,
				"path", normalizedRoutePattern(r),
			)

			ctx := context.WithValue(r.Context(), loggerKey, logger)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// GinLoggingMiddleware returns a logger middleware compatible with the Gin.
func GinLoggingMiddleware(serviceName string) gin.HandlerFunc {
	middleware := LoggingMiddleware(serviceName)

	return func(c *gin.Context) {
		middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.Request = r
			c.Next()
		})).ServeHTTP(c.Writer, c.Request)
	}
}

func normalizedRoutePattern(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}

	return r.URL.Path
}
