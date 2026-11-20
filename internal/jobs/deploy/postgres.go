package deploy

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/zedensix/homerun/internal/pgupgrade"
	"github.com/zedensix/homerun/internal/tracing"
)

// upgradePostgres moves the service's Postgres data onto the resolved image's
// major when it switches majors (see pgupgrade). The service's running
// containers or swarm tasks are stopped first so the dump is consistent and
// no old server writes behind it, and stay stopped once it succeeded, since
// whatever the old version wrote after the dump would be lost. A failed
// upgrade starts them
// again, the old cluster untouched. An image the manager can't inspect is
// left alone.
func (r *run) upgradePostgres(ctx context.Context, resolved resolvedImage) error {
	var volumes []pgupgrade.Volume
	for _, volume := range r.spec.Volumes {
		if !volume.ReadOnly && volume.Source != "" && volume.Target != "" {
			volumes = append(volumes, pgupgrade.Volume{Source: volume.Source, Target: volume.Target})
		}
	}
	if len(volumes) == 0 {
		return nil
	}
	if !r.isPostgres(ctx, resolved.ref()) {
		return nil
	}
	env, err := r.workloadEnv(ctx)
	if err != nil {
		return err
	}
	request := pgupgrade.Request{Env: env, Image: resolved.ref(), Previous: r.spec.Previous, Volumes: volumes}
	plan, err := pgupgrade.Find(ctx, r.docker, request)
	if err != nil || plan == nil {
		return err
	}
	return tracing.Stage(ctx, "upgrade postgres", func(ctx context.Context) error {
		running, err := r.runningWorkloads(ctx)
		if err != nil {
			return err
		}
		var stopped []stoppable
		upgradeErr := func() error {
			for _, workload := range running {
				if err := workload.stop(ctx); err != nil {
					return fmt.Errorf("couldn't stop %s before upgrading Postgres: %w", workload.name, err)
				}
				stopped = append(stopped, workload)
				r.progress.line(fmt.Sprintf("Stopped %s while Postgres is upgraded", workload.name))
			}
			return plan.Run(ctx, r.docker, request, r.progress.line)
		}()
		if upgradeErr == nil {
			return nil
		}
		for _, workload := range stopped {
			if err := workload.start(context.WithoutCancel(ctx)); err != nil {
				r.progress.line(fmt.Sprintf("Couldn't start %s again after the failed upgrade: %s", workload.name, err))
				continue
			}
			r.progress.line(fmt.Sprintf("Started %s again on Postgres %s", workload.name, plan.OldVersion))
		}
		return upgradeErr
	})
}

// isPostgres reports whether ref is a Postgres image, by the PG_MAJOR the
// official image and the images built on it set. An image the daemon can't
// inspect isn't.
func (r *run) isPostgres(ctx context.Context, ref string) bool {
	inspected, err := r.docker.InspectImage(ctx, ref)
	return err == nil && slices.ContainsFunc(inspected.Config.Env, func(entry string) bool {
		return strings.HasPrefix(entry, "PG_MAJOR=")
	})
}
