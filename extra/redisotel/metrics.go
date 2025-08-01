package redisotel

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/redis/go-redis/v9"
)

type metricsState struct {
	registrations []metric.Registration
	closed        bool
	mutex         sync.Mutex
}

// InstrumentMetrics starts reporting OpenTelemetry Metrics.
//
// Based on https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/metrics/semantic_conventions/database-metrics.md
func InstrumentMetrics(rdb redis.UniversalClient, opts ...MetricsOption) error {
	baseOpts := make([]baseOption, len(opts))
	for i, opt := range opts {
		baseOpts[i] = opt
	}
	conf := newConfig(baseOpts...)

	if conf.meter == nil {
		conf.meter = conf.mp.Meter(
			instrumName,
			metric.WithInstrumentationVersion("semver:"+redis.Version()),
		)
	}

	var state *metricsState
	if conf.closeChan != nil {
		state = &metricsState{
			registrations: make([]metric.Registration, 0),
			closed:        false,
			mutex:         sync.Mutex{},
		}

		go func() {
			<-conf.closeChan

			state.mutex.Lock()
			state.closed = true

			for _, registration := range state.registrations {
				if err := registration.Unregister(); err != nil {
					otel.Handle(err)
				}
			}
			state.mutex.Unlock()
		}()
	}

	switch rdb := rdb.(type) {
	case *redis.Client:
		return registerClient(rdb, conf, state)
	case *redis.ClusterClient:
		rdb.OnNewNode(func(rdb *redis.Client) {
			if err := registerClient(rdb, conf, state); err != nil {
				otel.Handle(err)
			}
		})
		return nil
	case *redis.Ring:
		rdb.OnNewNode(func(rdb *redis.Client) {
			if err := registerClient(rdb, conf, state); err != nil {
				otel.Handle(err)
			}
		})
		return nil
	default:
		return fmt.Errorf("redisotel: %T not supported", rdb)
	}
}

func registerClient(rdb *redis.Client, conf *config, state *metricsState) error {
	if state != nil {
		state.mutex.Lock()
		defer state.mutex.Unlock()

		if state.closed {
			return nil
		}
	}

	if conf.poolName == "" {
		opt := rdb.Options()
		conf.poolName = opt.Addr
	}
	conf.attrs = append(conf.attrs, attribute.String("pool.name", conf.poolName))

	registration, err := reportPoolStats(rdb, conf)
	if err != nil {
		return err
	}

	if state != nil {
		state.registrations = append(state.registrations, registration)
	}

	if err := addMetricsHook(rdb, conf); err != nil {
		return err
	}
	return nil
}

func reportPoolStats(rdb *redis.Client, conf *config) (metric.Registration, error) {
	labels := conf.attrs
	idleAttrs := append(labels, attribute.String("state", "idle"))
	usedAttrs := append(labels, attribute.String("state", "used"))

	idleMax, err := conf.meter.Int64ObservableUpDownCounter(
		"db.client.connections.idle.max",
		metric.WithDescription("The maximum number of idle open connections allowed"),
	)
	if err != nil {
		return nil, err
	}

	idleMin, err := conf.meter.Int64ObservableUpDownCounter(
		"db.client.connections.idle.min",
		metric.WithDescription("The minimum number of idle open connections allowed"),
	)
	if err != nil {
		return nil, err
	}

	connsMax, err := conf.meter.Int64ObservableUpDownCounter(
		"db.client.connections.max",
		metric.WithDescription("The maximum number of open connections allowed"),
	)
	if err != nil {
		return nil, err
	}

	usage, err := conf.meter.Int64ObservableUpDownCounter(
		"db.client.connections.usage",
		metric.WithDescription("The number of connections that are currently in state described by the state attribute"),
	)
	if err != nil {
		return nil, err
	}

	timeouts, err := conf.meter.Int64ObservableUpDownCounter(
		"db.client.connections.timeouts",
		metric.WithDescription("The number of connection timeouts that have occurred trying to obtain a connection from the pool"),
	)
	if err != nil {
		return nil, err
	}

	hits, err := conf.meter.Int64ObservableUpDownCounter(
		"db.client.connections.hits",
		metric.WithDescription("The number of times free connection was found in the pool"),
	)
	if err != nil {
		return nil, err
	}

	misses, err := conf.meter.Int64ObservableUpDownCounter(
		"db.client.connections.misses",
		metric.WithDescription("The number of times free connection was not found in the pool"),
	)
	if err != nil {
		return nil, err
	}

	redisConf := rdb.Options()
	return conf.meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			stats := rdb.PoolStats()

			o.ObserveInt64(idleMax, int64(redisConf.MaxIdleConns), metric.WithAttributes(labels...))
			o.ObserveInt64(idleMin, int64(redisConf.MinIdleConns), metric.WithAttributes(labels...))
			o.ObserveInt64(connsMax, int64(redisConf.PoolSize), metric.WithAttributes(labels...))

			o.ObserveInt64(usage, int64(stats.IdleConns), metric.WithAttributes(idleAttrs...))
			o.ObserveInt64(usage, int64(stats.TotalConns-stats.IdleConns), metric.WithAttributes(usedAttrs...))

			o.ObserveInt64(timeouts, int64(stats.Timeouts), metric.WithAttributes(labels...))
			o.ObserveInt64(hits, int64(stats.Hits), metric.WithAttributes(labels...))
			o.ObserveInt64(misses, int64(stats.Misses), metric.WithAttributes(labels...))
			return nil
		},
		idleMax,
		idleMin,
		connsMax,
		usage,
		timeouts,
		hits,
		misses,
	)
}

func addMetricsHook(rdb *redis.Client, conf *config) error {
	createTime, err := conf.meter.Float64Histogram(
		"db.client.connections.create_time",
		metric.WithDescription("The time it took to create a new connection."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	useTime, err := conf.meter.Float64Histogram(
		"db.client.connections.use_time",
		metric.WithDescription("The time between borrowing a connection and returning it to the pool."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return err
	}

	rdb.AddHook(&metricsHook{
		createTime: createTime,
		useTime:    useTime,
		attrs:      conf.attrs,
	})
	return nil
}

type metricsHook struct {
	createTime metric.Float64Histogram
	useTime    metric.Float64Histogram
	attrs      []attribute.KeyValue
}

var _ redis.Hook = (*metricsHook)(nil)

func (mh *metricsHook) DialHook(hook redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		start := time.Now()

		conn, err := hook(ctx, network, addr)

		dur := time.Since(start)

		attrs := make([]attribute.KeyValue, 0, len(mh.attrs)+1)
		attrs = append(attrs, mh.attrs...)
		attrs = append(attrs, statusAttr(err))

		mh.createTime.Record(ctx, milliseconds(dur), metric.WithAttributes(attrs...))
		return conn, err
	}
}

func (mh *metricsHook) ProcessHook(hook redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()

		err := hook(ctx, cmd)

		dur := time.Since(start)

		attrs := make([]attribute.KeyValue, 0, len(mh.attrs)+2)
		attrs = append(attrs, mh.attrs...)
		attrs = append(attrs, attribute.String("type", "command"))
		attrs = append(attrs, statusAttr(err))

		mh.useTime.Record(ctx, milliseconds(dur), metric.WithAttributes(attrs...))

		return err
	}
}

func (mh *metricsHook) ProcessPipelineHook(
	hook redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()

		err := hook(ctx, cmds)

		dur := time.Since(start)

		attrs := make([]attribute.KeyValue, 0, len(mh.attrs)+2)
		attrs = append(attrs, mh.attrs...)
		attrs = append(attrs, attribute.String("type", "pipeline"))
		attrs = append(attrs, statusAttr(err))

		mh.useTime.Record(ctx, milliseconds(dur), metric.WithAttributes(attrs...))

		return err
	}
}

func milliseconds(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func statusAttr(err error) attribute.KeyValue {
	if err != nil {
		return attribute.String("status", "error")
	}
	return attribute.String("status", "ok")
}
