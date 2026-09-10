package report

import (
	"fmt"
	"io"

	"github.com/SamuelMolling/godwit/internal/config"
)

func checkPlanFormat(planFormat string) error {
	if planFormat != config.PlanFormatSchema && planFormat != config.PlanFormatStatements {
		return fmt.Errorf("unknown plan format %q (want schema or statements)", planFormat)
	}

	return nil
}

// PlanWriter renders a plan as format ("text", "markdown" or "json"), saying what planFormat asks for.
func PlanWriter(format, planFormat string) (func(io.Writer, Plan), error) {
	write, ok := planFormats[format]
	if !ok {
		return nil, fmt.Errorf("unknown format %q (want text, markdown or json)", format)
	}
	if err := checkPlanFormat(planFormat); err != nil {
		return nil, err
	}

	return func(w io.Writer, r Plan) {
		r.format = planFormat
		write(w, r)
	}, nil
}

// RunWriter renders a run as format ("text" or "markdown"), saying what planFormat asks for.
func RunWriter(format, planFormat string) (func(io.Writer, Run), error) {
	write, ok := runFormats[format]
	if !ok {
		return nil, fmt.Errorf("unknown format %q (want text or markdown)", format)
	}
	if err := checkPlanFormat(planFormat); err != nil {
		return nil, err
	}

	return func(w io.Writer, r Run) {
		r.plan.format = planFormat

		write(w, r)
	}, nil
}
