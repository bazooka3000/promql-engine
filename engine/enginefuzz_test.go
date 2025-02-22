// Copyright (c) The Thanos Community Authors.
// Licensed under the Apache License 2.0.

package engine_test

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/cortexproject/promqlsmith"
	"github.com/efficientgo/core/errors"
	"github.com/efficientgo/core/testutil"
	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/teststorage"
	"github.com/stretchr/testify/require"

	"github.com/thanos-io/promql-engine/api"
	"github.com/thanos-io/promql-engine/engine"
	"github.com/thanos-io/promql-engine/execution/parse"
	"github.com/thanos-io/promql-engine/logicalplan"
)

const testRuns = 100

type testCase struct {
	query          string
	load           string
	oldRes, newRes *promql.Result
}

func FuzzEnginePromQLSmithRangeQuery(f *testing.F) {
	f.Add(uint32(0), uint32(120), uint32(30), 1.0, 1.0, 1.0, 2.0, 30)

	f.Fuzz(func(t *testing.T, startTS, endTS, intervalSeconds uint32, initialVal1, initialVal2, inc1, inc2 float64, stepRange int) {
		if math.IsNaN(initialVal1) || math.IsNaN(initialVal2) || math.IsNaN(inc1) || math.IsNaN(inc2) {
			return
		}
		if math.IsInf(initialVal1, 0) || math.IsInf(initialVal2, 0) || math.IsInf(inc1, 0) || math.IsInf(inc2, 0) {
			return
		}
		if inc1 < 0 || inc2 < 0 || stepRange <= 0 || intervalSeconds <= 0 || endTS < startTS {
			return
		}

		load := fmt.Sprintf(`load 30s
			http_requests_total{pod="nginx-1"} %.2f+%.2fx15
			http_requests_total{pod="nginx-2"} %2.f+%.2fx21`, initialVal1, inc1, initialVal2, inc2)

		opts := promql.EngineOpts{
			Timeout:              1 * time.Hour,
			MaxSamples:           1e10,
			EnableNegativeOffset: true,
			EnableAtModifier:     true,
		}

		storage := promql.LoadedStorage(t, load)
		defer storage.Close()

		start := time.Unix(int64(startTS), 0)
		end := time.Unix(int64(endTS), 0)
		interval := time.Duration(intervalSeconds) * time.Second

		seriesSet, err := getSeries(context.Background(), storage)
		require.NoError(t, err)
		rnd := rand.New(rand.NewSource(time.Now().Unix()))
		psOpts := []promqlsmith.Option{
			promqlsmith.WithEnableOffset(true),
			promqlsmith.WithEnableAtModifier(true),
		}
		ps := promqlsmith.New(rnd, seriesSet, psOpts...)

		newEngine := engine.New(engine.Opts{EngineOpts: opts, DisableFallback: true})
		oldEngine := promql.NewEngine(opts)

		var (
			q1    promql.Query
			query string
		)
		cases := make([]*testCase, testRuns)
		for i := 0; i < testRuns; i++ {
			// Since we disabled fallback, keep trying until we find a query
			// that can be natively executed by the engine.
			for {
				expr := ps.WalkRangeQuery()
				query = expr.Pretty(0)
				q1, err = newEngine.NewRangeQuery(context.Background(), storage, nil, query, start, end, interval)
				if errors.Is(err, parse.ErrNotSupportedExpr) || errors.Is(err, parse.ErrNotImplemented) {
					continue
				} else {
					break
				}
			}

			testutil.Ok(t, err)
			newResult := q1.Exec(context.Background())

			q2, err := oldEngine.NewRangeQuery(context.Background(), storage, nil, query, start, end, interval)
			testutil.Ok(t, err)

			oldResult := q2.Exec(context.Background())

			cases[i] = &testCase{
				query:  query,
				newRes: newResult,
				oldRes: oldResult,
				load:   load,
			}
		}
		validateTestCases(t, cases)
	})
}

func validateTestCases(t *testing.T, cases []*testCase) {
	failures := 0
	for i, c := range cases {
		emptyLabelsToNil(c.newRes)
		emptyLabelsToNil(c.oldRes)

		if !cmp.Equal(c.oldRes, c.newRes, comparer) {
			t.Logf(c.load)
			t.Logf(c.query)

			t.Logf("case %d error mismatch.\nnew result: %s\nold result: %s\n", i, c.newRes.String(), c.oldRes.String())
			failures++
		}
	}
	if failures > 0 {
		t.Fatalf("failed %d test cases", failures)
	}
}

func FuzzEnginePromQLSmithInstantQuery(f *testing.F) {
	f.Add(uint32(0), 1.0, 1.0, 1.0, 2.0)

	f.Fuzz(func(t *testing.T, ts uint32, initialVal1, initialVal2, inc1, inc2 float64) {
		if inc1 < 0 || inc2 < 0 {
			return
		}
		load := fmt.Sprintf(`load 30s
			http_requests_total{pod="nginx-1", route="/"} %.2f+%.2fx40
			http_requests_total{pod="nginx-2", route="/"} %2.f+%.2fx40`, initialVal1, inc1, initialVal2, inc2)

		opts := promql.EngineOpts{
			Timeout:              1 * time.Hour,
			MaxSamples:           1e10,
			EnableNegativeOffset: true,
			EnableAtModifier:     true,
		}

		storage := promql.LoadedStorage(t, load)
		defer storage.Close()

		queryTime := time.Unix(int64(ts), 0)
		newEngine := engine.New(engine.Opts{
			EngineOpts:        opts,
			DisableFallback:   true,
			LogicalOptimizers: logicalplan.AllOptimizers,
		})
		oldEngine := promql.NewEngine(opts)

		seriesSet, err := getSeries(context.Background(), storage)
		require.NoError(t, err)
		rnd := rand.New(rand.NewSource(time.Now().Unix()))
		psOpts := []promqlsmith.Option{
			promqlsmith.WithEnableOffset(true),
			promqlsmith.WithEnableAtModifier(true),
			promqlsmith.WithAtModifierMaxTimestamp(180 * 1000),
		}
		ps := promqlsmith.New(rnd, seriesSet, psOpts...)

		var (
			q1    promql.Query
			query string
		)
		cases := make([]*testCase, testRuns)
		for i := 0; i < testRuns; i++ {
			// Since we disabled fallback, keep trying until we find a query
			// that can be natively execute by the engine.
			for {
				expr := ps.WalkInstantQuery()
				query = expr.Pretty(0)
				q1, err = newEngine.NewInstantQuery(context.Background(), storage, nil, query, queryTime)
				if errors.Is(err, parse.ErrNotSupportedExpr) || errors.Is(err, parse.ErrNotImplemented) {
					continue
				} else {
					break
				}
			}

			testutil.Ok(t, err)
			newResult := q1.Exec(context.Background())

			q2, err := oldEngine.NewInstantQuery(context.Background(), storage, nil, query, queryTime)
			testutil.Ok(t, err)

			oldResult := q2.Exec(context.Background())

			sortByLabels(newResult)
			sortByLabels(oldResult)

			cases[i] = &testCase{
				query:  query,
				newRes: newResult,
				oldRes: oldResult,
				load:   load,
			}
		}
		validateTestCases(t, cases)
	})
}

func FuzzDistributedEnginePromQLSmithRangeQuery(f *testing.F) {
	f.Add(uint32(0), uint32(120), uint32(30), 1.0, 1.0, 1.0, 1.0, 1.0, 2.0, 30)

	f.Fuzz(func(t *testing.T, startTS, endTS, intervalSeconds uint32, initialVal1, initialVal2, initialVal3, initialVal4, inc1, inc2 float64, stepRange int) {
		if math.IsNaN(initialVal1) || math.IsNaN(initialVal2) || math.IsNaN(inc1) || math.IsNaN(inc2) {
			return
		}
		if math.IsInf(initialVal1, 0) || math.IsInf(initialVal2, 0) || math.IsInf(inc1, 0) || math.IsInf(inc2, 0) {
			return
		}
		if inc1 < 0 || inc2 < 0 || stepRange <= 0 || intervalSeconds <= 0 || endTS < startTS {
			return
		}
		load := fmt.Sprintf(`load 30s
			http_requests_total{pod="nginx-1", route="/"} %.2f+%.2fx4
			http_requests_total{pod="nginx-2", route="/"} %2.f+%.2fx4`, initialVal1, inc1, initialVal2, inc2)
		load2 := fmt.Sprintf(`load 30s
			http_requests_total{pod="nginx-1", route="/"} %.2f+%.2fx4
			http_requests_total{pod="nginx-2", route="/"} %2.f+%.2fx4`, initialVal3, inc1, initialVal4, inc2)

		opts := promql.EngineOpts{
			Timeout:              1 * time.Hour,
			MaxSamples:           1e10,
			EnableNegativeOffset: true,
			EnableAtModifier:     true,
		}
		engineOpts := engine.Opts{
			EngineOpts:        opts,
			DisableFallback:   true,
			LogicalOptimizers: logicalplan.AllOptimizers,
		}

		queryables := []*teststorage.TestStorage{}
		storage1 := promql.LoadedStorage(t, load)
		defer storage1.Close()
		queryables = append(queryables, storage1)

		storage2 := promql.LoadedStorage(t, load2)
		defer storage2.Close()
		queryables = append(queryables, storage1)

		start := time.Unix(int64(startTS), 0)
		end := time.Unix(int64(endTS), 0)
		interval := time.Duration(intervalSeconds) * time.Second

		partitionLabels := [][]labels.Labels{
			{labels.FromStrings("zone", "west-1")},
			{labels.FromStrings("zone", "west-2")},
		}
		remoteEngines := make([]api.RemoteEngine, 0, 2)
		for i := 0; i < 2; i++ {
			e := engine.NewRemoteEngine(
				engineOpts,
				queryables[i],
				queryables[i].DB.Head().MinTime(),
				queryables[i].DB.Head().MaxTime(),
				partitionLabels[i],
			)
			remoteEngines = append(remoteEngines, e)
		}
		distEngine := engine.NewDistributedEngine(engineOpts, api.NewStaticEndpoints(remoteEngines))
		oldEngine := promql.NewEngine(opts)

		mergeStore := storage.NewFanout(nil, storage1, storage2)
		seriesSet, err := getSeries(context.Background(), mergeStore)
		require.NoError(t, err)
		rnd := rand.New(rand.NewSource(time.Now().Unix()))
		psOpts := []promqlsmith.Option{
			promqlsmith.WithEnableOffset(true),
			promqlsmith.WithEnableAtModifier(true),
			promqlsmith.WithAtModifierMaxTimestamp(180 * 1000),
			promqlsmith.WithEnabledAggrs([]parser.ItemType{parser.SUM, parser.MIN, parser.MAX, parser.GROUP, parser.COUNT, parser.BOTTOMK, parser.TOPK}),
		}
		ps := promqlsmith.New(rnd, seriesSet, psOpts...)

		var (
			q1    promql.Query
			query string
		)
		cases := make([]*testCase, testRuns)
		ctx := context.Background()
		for i := 0; i < testRuns; i++ {
			// Since we disabled fallback, keep trying until we find a query
			// that can be natively execute by the engine.
			for {
				expr := ps.WalkRangeQuery()
				query = expr.Pretty(0)
				q1, err = distEngine.NewRangeQuery(ctx, mergeStore, nil, query, start, end, interval)
				if errors.Is(err, parse.ErrNotSupportedExpr) || errors.Is(err, parse.ErrNotImplemented) {
					continue
				} else {
					break
				}
			}

			testutil.Ok(t, err)
			newResult := q1.Exec(ctx)

			q2, err := oldEngine.NewRangeQuery(ctx, mergeStore, nil, query, start, end, interval)
			testutil.Ok(t, err)

			oldResult := q2.Exec(ctx)

			cases[i] = &testCase{
				query:  query,
				newRes: newResult,
				oldRes: oldResult,
				load:   load,
			}
		}
		validateTestCases(t, cases)
	})
}

func FuzzDistributedEnginePromQLSmithInstantQuery(f *testing.F) {
	f.Add(uint32(0), 1.0, 1.0, 1.0, 1.0, 1.0, 2.0)

	f.Fuzz(func(t *testing.T, ts uint32, initialVal1, initialVal2, initialVal3, initialVal4, inc1, inc2 float64) {
		if inc1 < 0 || inc2 < 0 {
			return
		}
		load := fmt.Sprintf(`load 30s
			http_requests_total{pod="nginx-1", route="/"} %.2f+%.2fx4
			http_requests_total{pod="nginx-2", route="/"} %2.f+%.2fx4`, initialVal1, inc1, initialVal2, inc2)
		load2 := fmt.Sprintf(`load 30s
			http_requests_total{pod="nginx-1", route="/"} %.2f+%.2fx4
			http_requests_total{pod="nginx-2", route="/"} %2.f+%.2fx4`, initialVal3, inc1, initialVal4, inc2)

		opts := promql.EngineOpts{
			Timeout:              1 * time.Hour,
			MaxSamples:           1e10,
			EnableNegativeOffset: true,
			EnableAtModifier:     true,
		}
		engineOpts := engine.Opts{EngineOpts: opts, DisableFallback: true}

		queryables := []*teststorage.TestStorage{}
		storage1 := promql.LoadedStorage(t, load)
		defer storage1.Close()
		queryables = append(queryables, storage1)

		storage2 := promql.LoadedStorage(t, load2)
		defer storage2.Close()
		queryables = append(queryables, storage1)

		partitionLabels := [][]labels.Labels{
			{labels.FromStrings("zone", "west-1")},
			{labels.FromStrings("zone", "west-2")},
		}
		queryTime := time.Unix(int64(ts), 0)
		remoteEngines := make([]api.RemoteEngine, 0, 2)
		for i := 0; i < 2; i++ {
			e := engine.NewRemoteEngine(
				engineOpts,
				queryables[i],
				queryables[i].DB.Head().MinTime(),
				queryables[i].DB.Head().MaxTime(),
				partitionLabels[i],
			)
			remoteEngines = append(remoteEngines, e)
		}
		distEngine := engine.NewDistributedEngine(engineOpts, api.NewStaticEndpoints(remoteEngines))
		oldEngine := promql.NewEngine(opts)

		mergeStore := storage.NewFanout(nil, storage1, storage2)
		seriesSet, err := getSeries(context.Background(), mergeStore)
		require.NoError(t, err)
		rnd := rand.New(rand.NewSource(time.Now().Unix()))
		psOpts := []promqlsmith.Option{
			promqlsmith.WithEnableOffset(true),
			promqlsmith.WithEnableAtModifier(true),
			promqlsmith.WithAtModifierMaxTimestamp(180 * 1000),
			promqlsmith.WithEnabledAggrs([]parser.ItemType{parser.SUM, parser.MIN, parser.MAX, parser.GROUP, parser.COUNT, parser.BOTTOMK, parser.TOPK}),
		}
		ps := promqlsmith.New(rnd, seriesSet, psOpts...)
		ctx := context.Background()

		var (
			q1    promql.Query
			query string
		)
		cases := make([]*testCase, testRuns)
		for i := 0; i < testRuns; i++ {
			// Since we disabled fallback, keep trying until we find a query
			// that can be natively execute by the engine.
			for {
				expr := ps.Walk(parser.ValueTypeVector, parser.ValueTypeMatrix)
				query = expr.Pretty(0)
				q1, err = distEngine.NewInstantQuery(ctx, mergeStore, nil, query, queryTime)
				if errors.Is(err, parse.ErrNotSupportedExpr) || errors.Is(err, parse.ErrNotImplemented) {
					continue
				} else {
					break
				}
			}

			testutil.Ok(t, err)
			newResult := q1.Exec(ctx)

			q2, err := oldEngine.NewInstantQuery(ctx, mergeStore, nil, query, queryTime)
			testutil.Ok(t, err)

			oldResult := q2.Exec(ctx)

			cases[i] = &testCase{
				query:  query,
				newRes: newResult,
				oldRes: oldResult,
				load:   load,
			}
		}
		validateTestCases(t, cases)
	})
}

func getSeries(ctx context.Context, q storage.Queryable) ([]labels.Labels, error) {
	querier, err := q.Querier(0, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	res := make([]labels.Labels, 0)
	ss := querier.Select(ctx, false, &storage.SelectHints{Func: "series"}, labels.MustNewMatcher(labels.MatchEqual, "__name__", "http_requests_total"))
	for ss.Next() {
		lbls := ss.At().Labels()
		res = append(res, lbls)
	}
	if err := ss.Err(); err != nil {
		return nil, err
	}
	return res, nil
}
