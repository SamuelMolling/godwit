package githubapp

import (
	"strings"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/link"
	"github.com/SamuelMolling/godwit/internal/report"
)

func planned(cmd command, p project, res *godwitv1.PlanRunResponse, public string) done {
	write, err := report.PlanWriter("markdown", p.format)
	if err != nil {
		return refusedBy(cmd, p, err)
	}
	r := report.PlanFromProto(res)
	var b strings.Builder
	write(&b, r)
	d := done{
		project: p, body: b.String(), title: r.Verdict(),
		conclusion: conclusionSuccess, url: link.Plan(public, res.GetPlanId()),
	}
	if r.Hazards() > 0 {
		d.conclusion = conclusionAction
	}

	return d
}
