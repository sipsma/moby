package ops

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/moby/buildkit/worker"
	"github.com/pkg/errors"

	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver"
	"github.com/moby/buildkit/solver/pb"
	digest "github.com/opencontainers/go-digest"
)

const diffCacheType = "buildkit.diff.v0"

type diffOp struct {
	op     *pb.DiffOp
	worker worker.Worker
}

func NewDiffOp(v solver.Vertex, op *pb.Op_Diff, w worker.Worker) (solver.Op, error) {
	if err := llbsolver.ValidateOp(&pb.Op{Op: op}); err != nil {
		return nil, err
	}
	return &diffOp{
		op:     op.Diff,
		worker: w,
	}, nil
}

func (d *diffOp) CacheMap(ctx context.Context, group session.Group, index int) (*solver.CacheMap, bool, error) {
	dt, err := json.Marshal(struct {
		Type string
		Diff *pb.DiffOp
	}{
		Type: diffCacheType,
		Diff: d.op,
	})
	if err != nil {
		return nil, false, err
	}

	cm := &solver.CacheMap{
		Digest: digest.Digest(dt),
		Deps: make([]struct {
			Selector          digest.Digest
			ComputeDigestFunc solver.ResultBasedCacheFunc
			PreprocessFunc    solver.PreprocessFunc
		}, 2),
	}

	return cm, true, nil
}

func (d *diffOp) Exec(ctx context.Context, g session.Group, inputs []solver.Result) ([]solver.Result, error) {
	var curInput int

	var lowerRef cache.ImmutableRef
	var lowerRefID string
	if d.op.Lower.Input != pb.Empty {
		if lowerInp := inputs[curInput]; lowerInp != nil {
			wref, ok := lowerInp.Sys().(*worker.WorkerRef)
			if !ok {
				return nil, errors.Errorf("invalid lower reference for diff op %T", lowerInp.Sys())
			}
			lowerRef = wref.ImmutableRef
			if lowerRef != nil {
				lowerRefID = wref.ImmutableRef.ID()
			}
		} else {
			return nil, errors.New("invalid nil lower input for diff op")
		}
		curInput++
	}

	var upperRef cache.ImmutableRef
	var upperRefID string
	if d.op.Upper.Input != pb.Empty {
		if upperInp := inputs[curInput]; upperInp != nil {
			wref, ok := upperInp.Sys().(*worker.WorkerRef)
			if !ok {
				return nil, errors.Errorf("invalid upper reference for diff op %T", upperInp.Sys())
			}
			upperRef = wref.ImmutableRef
			if upperRef != nil {
				upperRefID = wref.ImmutableRef.ID()
			}
		} else {
			return nil, errors.New("invalid nil upper input for diff op")
		}
	}

	diffRef, err := d.worker.CacheManager().Diff(ctx, lowerRef, upperRef,
		cache.WithDescription(fmt.Sprintf("diff %q -> %q", lowerRefID, upperRefID)))
	if err != nil {
		return nil, err
	}

	return []solver.Result{worker.NewWorkerRefResult(diffRef, d.worker)}, nil
}

func (d *diffOp) Acquire(ctx context.Context) (release solver.ReleaseFunc, err error) {
	return func() {}, nil
}
