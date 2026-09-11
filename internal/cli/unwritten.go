package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/report"
)

func emptySet(dir string) ([]engine.Migration, string, error) {
	migs, err := engine.LoadDir(dir)
	switch {
	case errors.Is(err, engine.ErrNoDir):
		return nil, dir + " does not exist yet", nil
	case err != nil:
		return nil, "", err
	case len(migs) == 0:
		return nil, dir + " holds no migration", nil
	}

	return migs, "", nil
}

func startedTarget(ctx context.Context, client godwitv1connect.GodwitServiceClient, target, why string) error {
	st, err := client.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: target}))
	if err != nil {
		return fmt.Errorf("%s, and %s could not be asked whether it already has migrations applied: %w",
			why, target, apiError(err))
	}
	if n := len(st.Msg.GetApplied()); n > 0 {
		return fmt.Errorf("%s, and %s already has %s applied: check --dir, the checkout and the branch",
			why, target, report.Count(n, "migration"))
	}

	return nil
}

func nothingLocal(cmd *cobra.Command, exec *engine.Executor, why string) error {
	n, err := exec.AppliedCount(cmd.Context())
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%s, and the database already has %s applied: check --dir and the checkout", why, report.Count(n, "migration"))
	}
	fmt.Fprintln(cmd.OutOrStdout(), "no migration yet: "+why)

	return nil
}

func (f *clientFlags) nothingYet(cmd *cobra.Command, target, why string, write func(io.Writer, report.Plan)) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	if err := startedTarget(cmd.Context(), client, target, why); err != nil {
		return err
	}

	return f.nothingPlanned(cmd, target, why, write)
}

func (f *clientFlags) nothingPlanned(cmd *cobra.Command, target, why string, write func(io.Writer, report.Plan)) error {
	if f.json {
		f.print(cmd, &godwitv1.PlanRunResponse{Target: target}, "")

		return nil
	}
	write(cmd.OutOrStdout(), report.PlanNothing(target, why))

	return nil
}

func (f *clientFlags) nothingRun(cmd *cobra.Command, why string) error {
	if !f.json {
		fmt.Fprintln(cmd.OutOrStdout(), "no migration to run: "+why)
	}

	return nil
}
