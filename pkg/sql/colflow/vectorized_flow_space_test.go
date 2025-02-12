// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package colflow_test

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/col/coltypes"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/execerror"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/stretchr/testify/require"
)

func TestVectorizeInternalMemorySpaceError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)

	flowCtx := &execinfra.FlowCtx{
		Cfg:     &execinfra.ServerConfig{Settings: st},
		EvalCtx: &evalCtx,
	}

	oneInput := []execinfrapb.InputSyncSpec{
		{ColumnTypes: []types.T{*types.Int}},
	}
	twoInputs := []execinfrapb.InputSyncSpec{
		{ColumnTypes: []types.T{*types.Int}},
		{ColumnTypes: []types.T{*types.Int}},
	}

	testCases := []struct {
		desc string
		spec *execinfrapb.ProcessorSpec
	}{
		{
			desc: "CASE",
			spec: &execinfrapb.ProcessorSpec{
				Input: oneInput,
				Core: execinfrapb.ProcessorCoreUnion{
					Noop: &execinfrapb.NoopCoreSpec{},
				},
				Post: execinfrapb.PostProcessSpec{
					RenderExprs: []execinfrapb.Expression{{Expr: "CASE WHEN @1 = 1 THEN 1 ELSE 2 END"}},
				},
			},
		},
		{
			desc: "MERGE JOIN",
			spec: &execinfrapb.ProcessorSpec{
				Input: twoInputs,
				Core: execinfrapb.ProcessorCoreUnion{
					MergeJoiner: &execinfrapb.MergeJoinerSpec{},
				},
			},
		},
	}

	for _, tc := range testCases {
		for _, success := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s-success-expected-%t", tc.desc, success), func(t *testing.T) {
				inputs := []colexec.Operator{colexec.NewZeroOp(nil)}
				if len(tc.spec.Input) > 1 {
					inputs = append(inputs, colexec.NewZeroOp(nil))
				}
				memMon := mon.MakeMonitor("MemoryMonitor", mon.MemoryResource, nil, nil, 0, math.MaxInt64, st)
				if success {
					memMon.Start(ctx, nil, mon.MakeStandaloneBudget(math.MaxInt64))
				} else {
					memMon.Start(ctx, nil, mon.MakeStandaloneBudget(1))
				}
				defer memMon.Stop(ctx)
				acc := memMon.MakeBoundAccount()
				defer acc.Close(ctx)
				result, err := colexec.NewColOperator(
					ctx, flowCtx, tc.spec, inputs, &mon.BoundAccount{},
					true, /* useStreamingMemAccountForBuffering */
				)
				if err != nil {
					t.Fatal(err)
				}
				err = acc.Grow(ctx, int64(result.InternalMemUsage))
				if success {
					require.NoError(t, err, "expected success, found: ", err)
				} else {
					require.Error(t, err, "expected memory error, found nothing")
				}
			})
		}
	}
}

func TestVectorizeAllocatorSpaceError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)

	flowCtx := &execinfra.FlowCtx{
		Cfg:     &execinfra.ServerConfig{Settings: st},
		EvalCtx: &evalCtx,
	}

	oneInput := []execinfrapb.InputSyncSpec{
		{ColumnTypes: []types.T{*types.Int}},
	}
	twoInputs := []execinfrapb.InputSyncSpec{
		{ColumnTypes: []types.T{*types.Int}},
		{ColumnTypes: []types.T{*types.Int}},
	}

	testCases := []struct {
		desc string
		spec *execinfrapb.ProcessorSpec
	}{
		{
			desc: "SORTER",
			spec: &execinfrapb.ProcessorSpec{
				Input: oneInput,
				Core: execinfrapb.ProcessorCoreUnion{
					Sorter: &execinfrapb.SorterSpec{
						OutputOrdering: execinfrapb.Ordering{
							Columns: []execinfrapb.Ordering_Column{
								{ColIdx: 0, Direction: execinfrapb.Ordering_Column_ASC},
							},
						},
					},
				},
			},
		},
		{
			desc: "HASH AGGREGATOR",
			spec: &execinfrapb.ProcessorSpec{
				Input: oneInput,
				Core: execinfrapb.ProcessorCoreUnion{
					Aggregator: &execinfrapb.AggregatorSpec{
						Type: execinfrapb.AggregatorSpec_SCALAR,
						Aggregations: []execinfrapb.AggregatorSpec_Aggregation{
							{
								Func:   execinfrapb.AggregatorSpec_MAX,
								ColIdx: []uint32{0},
							},
						},
					},
				},
			},
		},
		{
			desc: "HASH JOINER",
			spec: &execinfrapb.ProcessorSpec{
				Input: twoInputs,
				Core: execinfrapb.ProcessorCoreUnion{
					HashJoiner: &execinfrapb.HashJoinerSpec{
						LeftEqColumns:  []uint32{0},
						RightEqColumns: []uint32{0},
					},
				},
			},
		},
	}

	batch := testAllocator.NewMemBatchWithSize(
		[]coltypes.T{coltypes.Int64}, 1, /* size */
	)
	for _, tc := range testCases {
		for _, success := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s-success-expected-%t", tc.desc, success), func(t *testing.T) {
				inputs := []colexec.Operator{colexec.NewRepeatableBatchSource(batch)}
				if len(tc.spec.Input) > 1 {
					inputs = append(inputs, colexec.NewRepeatableBatchSource(batch))
				}
				memMon := mon.MakeMonitor("MemoryMonitor", mon.MemoryResource, nil, nil, 0, math.MaxInt64, st)
				if success {
					memMon.Start(ctx, nil, mon.MakeStandaloneBudget(math.MaxInt64))
				} else {
					memMon.Start(ctx, nil, mon.MakeStandaloneBudget(1))
				}
				defer memMon.Stop(ctx)
				acc := memMon.MakeBoundAccount()
				defer acc.Close(ctx)
				result, err := colexec.NewColOperator(
					ctx, flowCtx, tc.spec, inputs, &acc,
					true, /* useStreamingMemAccountForBuffering */
				)
				require.NoError(t, err)
				err = execerror.CatchVectorizedRuntimeError(func() {
					result.Op.Init()
					result.Op.Next(ctx)
				})
				if success {
					require.NoError(t, err, "expected success, found: ", err)
				} else {
					require.Error(t, err, "expected memory error, found nothing")
				}
			})
		}
	}
}
