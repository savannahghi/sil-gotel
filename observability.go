package silgotel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
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
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	otelTrace "go.opentelemetry.io/otel/trace"
)

// setupOtelSDK bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func (c *Client) setupOtelSDK(ctx context.Context) (func(context.Context) error, error) {
	var (
		shutdownFuncs []func(context.Context) error
		err           error
	)

	// shutdown calls cleanup functions registered via shutdownFuncs.
	// The errors from the calls are joined.
	// Each registered cleanup will be invoked once.
	shutdown := func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}

		shutdownFuncs = nil

		return err
	}

	// handleErr calls shutdown for cleanup and makes sure that all errors are returned.
	handleErr := func(inErr error) {
		err = errors.Join(inErr, shutdown(ctx))
	}

	// Set up propagator.
	prop := newPropagator()
	otel.SetTextMapPropagator(prop)

	// Set up trace provider.
	tracerProvider, err := c.newTracerProvider(ctx)
	if err != nil {
		handleErr(err)

		return shutdown, err
	}

	shutdownFuncs = append(shutdownFuncs, tracerProvider.Shutdown)
	otel.SetTracerProvider(tracerProvider)

	meterProvider, err := c.newMeterProvider(ctx)
	if err != nil {
		handleErr(err)

		return shutdown, err
	}

	shutdownFuncs = append(shutdownFuncs, meterProvider.Shutdown)
	otel.SetMeterProvider(meterProvider)

	// Set up logger provider.
	loggerProvider, err := c.newLoggerProvider(ctx)
	if err != nil {
		handleErr(err)

		return shutdown, err
	}

	shutdownFuncs = append(shutdownFuncs, loggerProvider.Shutdown)
	// Register as global logger provider so that it can be accessed global.LoggerProvider.
	// Most log bridges use the global logger provider as default.
	// If the global logger provider is not set then a no-op implementation
	// is used, which fails to generate data.
	global.SetLoggerProvider(loggerProvider)

	return shutdown, err
}

//nolint:ireturn
func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

func (c *Client) newTracerProvider(ctx context.Context) (*trace.TracerProvider, error) {
	headers := map[string]string{
		"content-type": "application/json",
	}

	exporter, err := otlptrace.New(
		ctx,
		otlptracehttp.NewClient(
			otlptracehttp.WithEndpointURL(fmt.Sprintf("%s/v1/traces", c.OTLPBaseURL)),
			otlptracehttp.WithHeaders(headers),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("creating new exporter: %w", err)
	}

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceNameKey.String(c.ServiceName),
		semconv.ServiceVersion(c.Version),
		semconv.DeploymentEnvironmentName(c.Environment),
	)

	tracerProvider := trace.NewTracerProvider(
		trace.WithBatcher(
			exporter,
			trace.WithMaxExportBatchSize(trace.DefaultMaxExportBatchSize),
			trace.WithBatchTimeout(trace.DefaultScheduleDelay),
			trace.WithExportTimeout(10*time.Second),
		),
		trace.WithResource(res),
	)

	otel.SetTracerProvider(tracerProvider)
	_ = tracerProvider.Tracer(c.ServiceName)

	return tracerProvider, nil
}

func (c *Client) newMeterProvider(ctx context.Context) (*sdkmetric.MeterProvider, error) {
	headers := map[string]string{
		"content-type": "application/json",
	}

	metricExporter, err := otlpmetrichttp.New(
		ctx,
		otlpmetrichttp.WithEndpointURL(fmt.Sprintf("%s/v1/metrics", c.OTLPBaseURL)),
		otlpmetrichttp.WithHeaders(headers),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(c.ServiceName),
			semconv.ServiceVersion(c.Version),
			semconv.DeploymentEnvironmentName(c.Environment),
		),
	)
	if err != nil {
		return nil, err
	}

	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(
				metricExporter,
				sdkmetric.WithInterval(5*time.Second),
				sdkmetric.WithTimeout(10*time.Second),
			),
		),
		WithHTTPViews(),
	)

	return meterProvider, nil
}

//nolint:ireturn
func WithHTTPViews() sdkmetric.Option {
	return sdkmetric.WithView(
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name: "http.server.request.duration",
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
	)
}

func (c *Client) newLoggerProvider(ctx context.Context) (*log.LoggerProvider, error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(c.ServiceName),
			semconv.ServiceVersion(c.Version),
			semconv.DeploymentEnvironmentName(c.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to merge resource: %w", err)
	}

	exporter, err := otlploghttp.New(
		ctx,
		otlploghttp.WithEndpointURL(fmt.Sprintf("%s/v1/logs", c.OTLPBaseURL)),
	)
	if err != nil {
		return nil, err
	}

	processor := log.NewBatchProcessor(exporter)
	provider := log.NewLoggerProvider(
		log.WithResource(res),
		log.WithProcessor(processor),
	)

	return provider, nil
}

func Trace(ctx context.Context, packageName, spanName string) (context.Context, otelTrace.Span) { //nolint: ireturn
	tracer := otel.Tracer(packageName)

	ctx, span := tracer.Start(ctx, spanName)
	defer span.End()

	return ctx, span
}

func Logger(ctx context.Context, packageName, message string) {
	otelLogger := otelslog.NewLogger(packageName)
	otelLogger.ErrorContext(ctx, message)
}

func CaptureTraceStatusAndError(span otelTrace.Span, err error) {
	span.SetStatus(codes.Error, err.Error())
	span.RecordError(err)
}

func CaptureMetrics(serviceName string) otelMetric.Meter { //nolint: ireturn
	return otel.Meter(serviceName)
}
