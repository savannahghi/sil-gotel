package silgotel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
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
				Name: "http.server.request_duration_ms",
				Kind: sdkmetric.InstrumentKindHistogram,
			},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: []float64{
						5, 10, 25, 50, 100,
						250, 500, 1000, 2500, 5000, 10000,
					},
				},
				Unit: "ms",
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
}

func newHTTPMetrics(serviceName string) *httpMetrics {
	meter := Meter(serviceName)

	return &httpMetrics{
		requestCounter: mustInstrument(meter.Int64Counter(
			"http.server.requests_total",
			otelMetric.WithDescription("Total number of HTTP requests processed"),
		)),
		requestDuration: mustInstrument(meter.Float64Histogram(
			"http.server.request_duration_ms",
			otelMetric.WithDescription("Request duration in milliseconds"),
			otelMetric.WithUnit("ms"),
		)),
		activeRequests: mustInstrument(meter.Int64UpDownCounter(
			"http.server.active_requests",
			otelMetric.WithDescription("Number of active HTTP server requests"),
			otelMetric.WithUnit("1"),
		)),
		requestSize: mustInstrument(meter.Int64Histogram(
			"http.server.request.body.size",
			otelMetric.WithDescription("Size of HTTP server request bodies"),
			otelMetric.WithUnit("By"),
		)),
		responseSize: mustInstrument(meter.Int64Histogram(
			"http.server.response.body.size",
			otelMetric.WithDescription("Size of HTTP server response bodies"),
			otelMetric.WithUnit("By"),
		)),
	}
}

// RequestMetrics returns a standard net/http middleware that records HTTP server metrics.
// The serviceName should match the service name used in NewOtelSDK.
func RequestMetrics(serviceName string) func(http.Handler) http.Handler {
	m := newHTTPMetrics(serviceName)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scheme := r.Header.Get("X-Forwarded-Proto")
			if scheme == "" {
				scheme = "http"
			}

			route := r.URL.Path // consumers can enrich this via context if needed

			baseAttrs := otelMetric.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.route", route),
				attribute.String("url.scheme", scheme),
			)

			ctx := r.Context()
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

			durationMs := float64(time.Since(start).Milliseconds())

			m.requestCounter.Add(ctx, 1, endAttrs)
			m.requestDuration.Record(ctx, durationMs, endAttrs)
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
