package silgotel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// nolint: ireturn
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

// nolint: godoclint,ireturn
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
// nolint: ireturn
func Trace(ctx context.Context, packageName, spanName string) (context.Context, otelTrace.Span) { //nolint:ireturn
	tracer := otel.Tracer(packageName)
	ctx, span := tracer.Start(ctx, spanName) //nolint:spancheck

	//nolint:spancheck
	return ctx, span
}

// NewLogger returns a reusable slog.Logger bridged to OTel. Cache the result.
func NewLogger(packageName string) *slog.Logger {
	return otelslog.NewLogger(packageName)
}

// RecordError sets the span status to error and records the error event.
func RecordError(span otelTrace.Span, err error) {
	if err == nil {
		return
	}

	span.SetStatus(codes.Error, err.Error())
	span.RecordError(err)
}

// Meter returns a named meter for recording metrics.
//
//nolint:ireturn
func Meter(serviceName string) otelMetric.Meter {
	return otel.Meter(serviceName)
}
