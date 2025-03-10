// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package internal

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config/configretry"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/exporterbatcher"
	"go.opentelemetry.io/collector/exporter/exporterqueue"
	"go.opentelemetry.io/collector/exporter/exportertest"
	"go.opentelemetry.io/collector/exporter/internal"
	"go.opentelemetry.io/collector/exporter/internal/requesttest"
	"go.opentelemetry.io/collector/pipeline"
)

var (
	defaultType     = component.MustNewType("test")
	defaultSignal   = pipeline.SignalMetrics
	defaultID       = component.NewID(defaultType)
	defaultSettings = func() exporter.Settings {
		set := exportertest.NewNopSettings()
		set.ID = defaultID
		return set
	}()
)

type noopSender struct {
	component.StartFunc
	component.ShutdownFunc
	SendFunc[internal.Request]
}

func newNoopExportSender() Sender[internal.Request] {
	return &noopSender{SendFunc: func(ctx context.Context, req internal.Request) error {
		select {
		case <-ctx.Done():
			return ctx.Err() // Returns the cancellation error
		default:
			return req.Export(ctx)
		}
	}}
}

func newNoopObsrepSender(_ *ObsReport, next Sender[internal.Request]) Sender[internal.Request] {
	return &noopSender{SendFunc: next.Send}
}

func TestBaseExporter(t *testing.T) {
	runTest := func(testName string, enableQueueBatcher bool) {
		t.Run(testName, func(t *testing.T) {
			defer setFeatureGateForTest(t, usePullingBasedExporterQueueBatcher, enableQueueBatcher)()
			be, err := NewBaseExporter(defaultSettings, defaultSignal, newNoopObsrepSender)
			require.NoError(t, err)
			require.NoError(t, be.Start(context.Background(), componenttest.NewNopHost()))
			require.NoError(t, be.Shutdown(context.Background()))
		})
	}
	runTest("enable_queue_batcher", true)
	runTest("disable_queue_batcher", false)
}

func TestBaseExporterWithOptions(t *testing.T) {
	runTest := func(testName string, enableQueueBatcher bool) {
		t.Run(testName, func(t *testing.T) {
			defer setFeatureGateForTest(t, usePullingBasedExporterQueueBatcher, enableQueueBatcher)()
			want := errors.New("my error")
			be, err := NewBaseExporter(
				defaultSettings, defaultSignal, newNoopObsrepSender,
				WithStart(func(context.Context, component.Host) error { return want }),
				WithShutdown(func(context.Context) error { return want }),
				WithTimeout(NewDefaultTimeoutConfig()),
			)
			require.NoError(t, err)
			require.Equal(t, want, be.Start(context.Background(), componenttest.NewNopHost()))
			require.Equal(t, want, be.Shutdown(context.Background()))
		})
	}
	runTest("enable_queue_batcher", true)
	runTest("disable_queue_batcher", false)
}

func TestQueueOptionsWithRequestExporter(t *testing.T) {
	runTest := func(testName string, enableQueueBatcher bool) {
		t.Run(testName, func(t *testing.T) {
			defer setFeatureGateForTest(t, usePullingBasedExporterQueueBatcher, enableQueueBatcher)()
			bs, err := NewBaseExporter(exportertest.NewNopSettings(), defaultSignal, newNoopObsrepSender,
				WithRetry(configretry.NewDefaultBackOffConfig()))
			require.NoError(t, err)
			require.Nil(t, bs.Marshaler)
			require.Nil(t, bs.Unmarshaler)
			_, err = NewBaseExporter(exportertest.NewNopSettings(), defaultSignal, newNoopObsrepSender,
				WithRetry(configretry.NewDefaultBackOffConfig()), WithQueue(NewDefaultQueueConfig()))
			require.Error(t, err)

			_, err = NewBaseExporter(exportertest.NewNopSettings(), defaultSignal, newNoopObsrepSender,
				WithMarshaler(mockRequestMarshaler), WithUnmarshaler(mockRequestUnmarshaler(&requesttest.FakeRequest{Items: 1})),
				WithRetry(configretry.NewDefaultBackOffConfig()),
				WithRequestQueue(exporterqueue.NewDefaultConfig(), exporterqueue.NewMemoryQueueFactory[internal.Request]()))
			require.Error(t, err)
		})
	}
	runTest("enable_queue_batcher", true)
	runTest("disable_queue_batcher", false)
}

func TestBaseExporterLogging(t *testing.T) {
	runTest := func(testName string, enableQueueBatcher bool) {
		t.Run(testName, func(t *testing.T) {
			defer setFeatureGateForTest(t, usePullingBasedExporterQueueBatcher, enableQueueBatcher)()
			set := exportertest.NewNopSettings()
			logger, observed := observer.New(zap.DebugLevel)
			set.Logger = zap.New(logger)
			rCfg := configretry.NewDefaultBackOffConfig()
			rCfg.Enabled = false
			qCfg := exporterqueue.NewDefaultConfig()
			qCfg.Enabled = false
			bs, err := NewBaseExporter(set, defaultSignal, newNoopObsrepSender,
				WithRequestQueue(qCfg, exporterqueue.NewMemoryQueueFactory[internal.Request]()),
				WithBatcher(exporterbatcher.NewDefaultConfig()),
				WithRetry(rCfg))
			require.NoError(t, err)
			require.NoError(t, bs.Start(context.Background(), componenttest.NewNopHost()))
			sink := requesttest.NewSink()
			sendErr := bs.Send(context.Background(), &requesttest.FakeRequest{Items: 2, Sink: sink, ExportErr: errors.New("my error")})
			require.Error(t, sendErr)

			require.Len(t, observed.FilterLevelExact(zap.ErrorLevel).All(), 2)
			assert.Contains(t, observed.All()[0].Message, "Exporting failed. Dropping data.")
			assert.Equal(t, "my error", observed.All()[0].ContextMap()["error"])
			assert.Contains(t, observed.All()[1].Message, "Exporting failed. Rejecting data.")
			assert.Equal(t, "my error", observed.All()[1].ContextMap()["error"])
			require.NoError(t, bs.Shutdown(context.Background()))
		})
	}
	runTest("enable_queue_batcher", true)
	runTest("disable_queue_batcher", false)
}

func TestQueueRetryWithDisabledQueue(t *testing.T) {
	tests := []struct {
		name         string
		queueOptions []Option
	}{
		{
			name: "WithQueue",
			queueOptions: []Option{
				WithMarshaler(mockRequestMarshaler),
				WithUnmarshaler(mockRequestUnmarshaler(&requesttest.FakeRequest{Items: 1})),
				func() Option {
					qs := NewDefaultQueueConfig()
					qs.Enabled = false
					return WithQueue(qs)
				}(),
				func() Option {
					bs := exporterbatcher.NewDefaultConfig()
					bs.Enabled = false
					return WithBatcher(bs)
				}(),
			},
		},
		{
			name: "WithRequestQueue",
			queueOptions: []Option{
				func() Option {
					qs := exporterqueue.NewDefaultConfig()
					qs.Enabled = false
					return WithRequestQueue(qs, exporterqueue.NewMemoryQueueFactory[internal.Request]())
				}(),
				func() Option {
					bs := exporterbatcher.NewDefaultConfig()
					bs.Enabled = false
					return WithBatcher(bs)
				}(),
			},
		},
	}

	runTest := func(testName string, enableQueueBatcher bool, tt struct {
		name         string
		queueOptions []Option
	},
	) {
		t.Run(testName, func(t *testing.T) {
			defer setFeatureGateForTest(t, usePullingBasedExporterQueueBatcher, enableQueueBatcher)()
			set := exportertest.NewNopSettings()
			logger, observed := observer.New(zap.ErrorLevel)
			set.Logger = zap.New(logger)
			be, err := NewBaseExporter(set, pipeline.SignalLogs, newObservabilityConsumerSender, tt.queueOptions...)
			require.NoError(t, err)
			require.NoError(t, be.Start(context.Background(), componenttest.NewNopHost()))
			ocs := be.ObsrepSender.(*observabilityConsumerSender)
			mockR := &requesttest.FakeRequest{Items: 2, ExportErr: errors.New("some error")}
			ocs.run(func() {
				require.Error(t, be.Send(context.Background(), mockR))
			})
			assert.Len(t, observed.All(), 1)
			assert.Equal(t, "Exporting failed. Rejecting data. Try enabling sending_queue to survive temporary failures.", observed.All()[0].Message)
			ocs.awaitAsyncProcessing()
			ocs.checkSendItemsCount(t, 0)
			ocs.checkDroppedItemsCount(t, 2)
			require.NoError(t, be.Shutdown(context.Background()))
		})
	}
	for _, tt := range tests {
		runTest(tt.name+"_enable_queue_batcher", true, tt)
		runTest(tt.name+"_disable_queue_batcher", false, tt)
	}
}
