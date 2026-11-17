package indexer

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/errs"
)

// executeParallel runs the plan's indexers with up to plan.MaxParallel of
// them at once (accuracy.indexers.max_parallel in user config, which was
// parsed and then ignored). It is opt-in on purpose: scip-typescript and
// scip-python each hold a Node heap of up to NodeHeapMB, so two at once on a
// small machine can exhaust memory, and the default of one keeps the
// sequential path in Execute exactly as it was.
//
// Installation stays sequential and comes first, because it can prompt. The
// results keep plan order whatever order the runs finish in, so a caller sees
// the same Indexes and Skipped either way. In exact mode the first failure in
// plan order is returned, and the runs still in flight are cancelled.
func executeParallel(ctx context.Context, plan Plan, runnable []Step, prov *Provisioner, exec *Executor, res Results, log *slog.Logger) (Results, error) {

	type job struct {
		step Step
		inst Installed
	}
	var jobs []job
	for _, s := range runnable {
		if err := ctx.Err(); err != nil {
			return res, errs.Wrap(errs.KindCancelled, "indexer.Execute", err)
		}
		inst := s.Installed
		if inst.Path == "" && inst.Image == "" {
			var err error
			inst, err = prov.Ensure(ctx, s.Pin)
			if err != nil {
				if plan.Accuracy == config.AccuracyExact {
					return res, errs.Wrap(errs.KindOf(err), "indexer.Execute",
						fmt.Errorf("accuracy.mode=exact needs %s for %s: %w", s.Pin.Indexer, s.Project, err))
				}
				res.Skipped = append(res.Skipped, Skip{
					Project: s.Project, Indexer: s.Indexer,
					Reason: "could not install " + s.Pin.Ref(), Err: err,
				})
				continue
			}
			if !inst.Preexisting {
				res.Provisioned = append(res.Provisioned, inst)
			}
		}
		jobs = append(jobs, job{step: s, inst: inst})
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		out Execution
		err error
		// genuine is false for a run that failed because a failure elsewhere
		// cancelled it (exact mode): reporting that error would hide the
		// indexer that actually failed.
		genuine bool
	}
	outcomes := make([]outcome, len(jobs))
	sem := make(chan struct{}, plan.MaxParallel)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-runCtx.Done():
				outcomes[i] = outcome{err: errs.Wrap(errs.KindCancelled, "indexer.Execute", runCtx.Err())}
				return
			}
			defer func() { <-sem }()
			log.Info("running indexer",
				"indexer", string(j.step.Indexer),
				"language", string(j.step.Project.Language),
				"project_dir", j.step.Project.Dir)
			out, err := exec.Run(runCtx, j.inst, j.step.Project, plan.RepoRoot, res.OutputDir)
			genuine := err != nil && (runCtx.Err() == nil || ctx.Err() != nil)
			outcomes[i] = outcome{out: out, err: err, genuine: genuine}
			if genuine && plan.Accuracy == config.AccuracyExact {
				cancel()
			}
		}(i, j)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return res, errs.Wrap(errs.KindCancelled, "indexer.Execute", err)
	}
	if plan.Accuracy == config.AccuracyExact {
		// The first failure in plan order that was not caused by our own
		// cancellation; the sequential path would have reported it.
		for _, o := range outcomes {
			if o.err != nil && o.genuine {
				return res, errs.Wrap(errs.KindOf(o.err), "indexer.Execute", o.err)
			}
		}
	}
	for i, o := range outcomes {
		s := jobs[i].step
		if o.err != nil {
			if plan.Accuracy == config.AccuracyExact {
				return res, errs.Wrap(errs.KindOf(o.err), "indexer.Execute", o.err)
			}
			res.Skipped = append(res.Skipped, Skip{
				Project: s.Project, Indexer: s.Indexer,
				Reason: failureReason(o.err), Err: o.err,
				Diagnostic: o.out.Diagnostic,
			})
			continue
		}
		res.Indexes = append(res.Indexes, o.out)
	}
	return res, nil
}
