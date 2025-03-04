// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package otlpmetricgrpc // import "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"

import (
	"context"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"go.opentelemetry.io/otel/exporters/otlp/internal/retry"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/internal/oconf"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/aggregation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/metric/view"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// New returns an OpenTelemetry metric Exporter. The Exporter can be used with
// a PeriodicReader to export OpenTelemetry metric data to an OTLP receiving
// endpoint using gRPC.
//
// If an already established gRPC ClientConn is not passed in options using
// WithGRPCConn, a connection to the OTLP endpoint will be established based
// on options. If a connection cannot be establishes in the lifetime of ctx,
// an error will be returned.
func New(ctx context.Context, options ...Option) (metric.Exporter, error) {
	c, err := newClient(ctx, options...)
	if err != nil {
		return nil, err
	}
	return otlpmetric.New(c), nil
}

type client struct {
	metadata      metadata.MD
	exportTimeout time.Duration
	requestFunc   retry.RequestFunc

	temporalitySelector metric.TemporalitySelector
	aggregationSelector metric.AggregationSelector

	// ourConn keeps track of where conn was created: true if created here in
	// NewClient, or false if passed with an option. This is important on
	// Shutdown as the conn should only be closed if we created it. Otherwise,
	// it is up to the processes that passed the conn to close it.
	ourConn bool
	conn    *grpc.ClientConn
	msc     colmetricpb.MetricsServiceClient
}

// newClient creates a new gRPC metric client.
func newClient(ctx context.Context, options ...Option) (otlpmetric.Client, error) {
	cfg := oconf.NewGRPCConfig(asGRPCOptions(options)...)

	c := &client{
		exportTimeout: cfg.Metrics.Timeout,
		requestFunc:   cfg.RetryConfig.RequestFunc(retryable),
		conn:          cfg.GRPCConn,

		temporalitySelector: cfg.Metrics.TemporalitySelector,
		aggregationSelector: cfg.Metrics.AggregationSelector,
	}

	if len(cfg.Metrics.Headers) > 0 {
		c.metadata = metadata.New(cfg.Metrics.Headers)
	}

	if c.conn == nil {
		// If the caller did not provide a ClientConn when the client was
		// created, create one using the configuration they did provide.
		conn, err := grpc.DialContext(ctx, cfg.Metrics.Endpoint, cfg.DialOptions...)
		if err != nil {
			return nil, err
		}
		// Keep track that we own the lifecycle of this conn and need to close
		// it on Shutdown.
		c.ourConn = true
		c.conn = conn
	}

	c.msc = colmetricpb.NewMetricsServiceClient(c.conn)

	return c, nil
}

// Temporality returns the Temporality to use for an instrument kind.
func (c *client) Temporality(k view.InstrumentKind) metricdata.Temporality {
	return c.temporalitySelector(k)
}

// Aggregation returns the Aggregation to use for an instrument kind.
func (c *client) Aggregation(k view.InstrumentKind) aggregation.Aggregation {
	return c.aggregationSelector(k)
}

// ForceFlush does nothing, the client holds no state.
func (c *client) ForceFlush(ctx context.Context) error { return ctx.Err() }

// Shutdown shuts down the client, freeing all resource.
//
// Any active connections to a remote endpoint are closed if they were created
// by the client. Any gRPC connection passed during creation using
// WithGRPCConn will not be closed. It is the caller's responsibility to
// handle cleanup of that resource.
func (c *client) Shutdown(ctx context.Context) error {
	// The otlpmetric.Exporter synchronizes access to client methods and
	// ensures this is called only once. The only thing that needs to be done
	// here is to release any computational resources the client holds.

	c.metadata = nil
	c.requestFunc = nil
	c.msc = nil

	err := ctx.Err()
	if c.ourConn {
		closeErr := c.conn.Close()
		// A context timeout error takes precedence over this error.
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}
	c.conn = nil
	return err
}

// UploadMetrics sends protoMetrics to connected endpoint.
//
// Retryable errors from the server will be handled according to any
// RetryConfig the client was created with.
func (c *client) UploadMetrics(ctx context.Context, protoMetrics *metricpb.ResourceMetrics) error {
	// The otlpmetric.Exporter synchronizes access to client methods, and
	// ensures this is not called after the Exporter is shutdown. Only thing
	// to do here is send data.

	select {
	case <-ctx.Done():
		// Do not upload if the context is already expired.
		return ctx.Err()
	default:
	}

	ctx, cancel := c.exportContext(ctx)
	defer cancel()

	return c.requestFunc(ctx, func(iCtx context.Context) error {
		_, err := c.msc.Export(iCtx, &colmetricpb.ExportMetricsServiceRequest{
			ResourceMetrics: []*metricpb.ResourceMetrics{protoMetrics},
		})
		// nil is converted to OK.
		if status.Code(err) == codes.OK {
			// Success.
			return nil
		}
		return err
	})
}

// exportContext returns a copy of parent with an appropriate deadline and
// cancellation function based on the clients configured export timeout.
//
// It is the callers responsibility to cancel the returned context once its
// use is complete, via the parent or directly with the returned CancelFunc, to
// ensure all resources are correctly released.
func (c *client) exportContext(parent context.Context) (context.Context, context.CancelFunc) {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	if c.exportTimeout > 0 {
		ctx, cancel = context.WithTimeout(parent, c.exportTimeout)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}

	if c.metadata.Len() > 0 {
		ctx = metadata.NewOutgoingContext(ctx, c.metadata)
	}

	return ctx, cancel
}

// retryable returns if err identifies a request that can be retried and a
// duration to wait for if an explicit throttle time is included in err.
func retryable(err error) (bool, time.Duration) {
	s := status.Convert(err)
	switch s.Code() {
	case codes.Canceled,
		codes.DeadlineExceeded,
		codes.ResourceExhausted,
		codes.Aborted,
		codes.OutOfRange,
		codes.Unavailable,
		codes.DataLoss:
		return true, throttleDelay(s)
	}

	// Not a retry-able error.
	return false, 0
}

// throttleDelay returns a duration to wait for if an explicit throttle time
// is included in the response status.
func throttleDelay(s *status.Status) time.Duration {
	for _, detail := range s.Details() {
		if t, ok := detail.(*errdetails.RetryInfo); ok {
			return t.RetryDelay.AsDuration()
		}
	}
	return 0
}
