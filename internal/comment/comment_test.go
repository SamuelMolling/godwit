package comment_test

import (
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/comment"
)

const head = "0123456789abcdef0123456789abcdef01234567"

func TestParseFound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
		want string
		cmd  comment.Command
	}{
		{"bare", "godwit apply", "apply", comment.Command{Name: "apply"}},
		{"slash alias", "/godwit apply", "apply", comment.Command{Name: "apply"}},
		{"any command", "godwit revert", "", comment.Command{Name: "revert"}},
		{"trimmed", "  godwit apply\r", "apply", comment.Command{Name: "apply"}},
		{"trailing blank lines", "godwit apply\n\n", "apply", comment.Command{Name: "apply"}},
		{"inline code", "`godwit apply`", "apply", comment.Command{Name: "apply"}},
		{"a one-line fence", "```godwit apply```", "apply", comment.Command{Name: "apply"}},
		{"backticks around the alias", "`/godwit apply --ack H001`", "apply", comment.Command{Name: "apply", Ack: []string{"H001"}}},
		{"sha", "godwit apply " + head, "apply", comment.Command{Name: "apply", Sha: head}},
		{"short sha", "godwit apply 0123456", "apply", comment.Command{Name: "apply", Sha: "0123456"}},
		{"ack", "godwit apply --ack H001", "apply", comment.Command{Name: "apply", Ack: []string{"H001"}}},
		{"ack assigned", "godwit apply --ack=H001", "apply", comment.Command{Name: "apply", Ack: []string{"H001"}}},
		{"ack list", "godwit apply --ack H001,H003", "apply", comment.Command{Name: "apply", Ack: []string{"H001", "H003"}}},
		{"sha then flag", "godwit apply " + head + " --ack H001", "apply", comment.Command{Name: "apply", Sha: head, Ack: []string{"H001"}}},
		{"plan rollout", "godwit plan --rollout expand-contract", "plan", comment.Command{Name: "plan", Rollout: "expand-contract"}},
		{"plan direct", "godwit plan --rollout=direct", "plan", comment.Command{Name: "plan", Rollout: "direct"}},
		{"plan bare", "godwit plan", "plan", comment.Command{Name: "plan"}},
		{"confirm", "godwit confirm " + head, "confirm", comment.Command{Name: "confirm", Sha: head}},
		{
			"revert flags", "godwit revert --ack H002 --allow-data-loss --force", "revert",
			comment.Command{Name: "revert", Ack: []string{"H002"}, AllowDataLoss: true, Force: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := comment.Parse(tc.body, tc.want)
			if err != nil {
				t.Fatalf("Parse(%q, %q) = %v", tc.body, tc.want, err)
			}
			if got == nil {
				t.Fatalf("Parse(%q, %q) found nothing", tc.body, tc.want)
			}
			if got.Name != tc.cmd.Name || got.Sha != tc.cmd.Sha || got.Rollout != tc.cmd.Rollout ||
				got.AllowDataLoss != tc.cmd.AllowDataLoss || got.Force != tc.cmd.Force ||
				strings.Join(got.Ack, ",") != strings.Join(tc.cmd.Ack, ",") {
				t.Fatalf("Parse(%q, %q) = %+v, want %+v", tc.body, tc.want, *got, tc.cmd)
			}
		})
	}
}

func TestParseSilence(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, body, want string }{
		{"empty", "", "apply"},
		{"prose", "looks good to me", "apply"},
		{"prose before, same line", "please godwit apply", "apply"},
		{"prose on a line above", "To deploy, comment:\ngodwit apply", "apply"},
		{"prose on a line below", "godwit apply\nthanks", "apply"},
		{"blank line between", "godwit apply\n\nthanks", "apply"},
		{"carriage returns", "deploy it:\r\ngodwit apply", "apply"},
		{"whitespace-only line between", "godwit apply\n \nthanks", "apply"},
		{"fenced", "```\ngodwit apply\n```", "apply"},
		{"tilde fenced", "~~~\ngodwit apply\n~~~", "apply"},
		{"unclosed fence", "```\ngodwit apply", "apply"},
		{"quoted log", "log:\n```\ngodwit apply\n```\ngodwit apply", "apply"},
		{"two commands", "godwit revert\ngodwit apply", ""},
		{"only backticks", "```", ""},
		{"a malformed command inside prose is silence, not a refusal", "note:\ngodwit apply --unknown", "apply"},
		{"another command", "godwit revert", "apply"},
		{"not a comment command", "godwit lint", ""},
		{"no command name", "godwit", ""},
		{"glued", "godwitapply", ""},
		{"double slash", "//godwit apply", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := comment.Parse(tc.body, tc.want)
			if err != nil || got != nil {
				t.Fatalf("Parse(%q, %q) = %+v, %v; want nil, nil", tc.body, tc.want, got, err)
			}
		})
	}
}

func TestParseRefusals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, body, want, msg string }{
		{"unknown want", "godwit apply", "lint", "'lint' is not a comment command"},
		{"bare word", "godwit apply notasha", "apply", "godwit apply does not understand 'notasha'"},
		{"long hex is not a sha", "godwit apply " + head + "0", "apply", "does not understand '" + head + "0'"},
		{"unknown flag", "godwit apply --unknown", "apply", "godwit apply does not understand '--unknown'"},
		{"value on a boolean flag", "godwit revert --force=1", "revert", "godwit revert does not understand '--force=1'"},
		{"another command's flag", "godwit apply --force", "apply", "godwit apply does not take --force"},
		{"confirm takes nothing", "godwit confirm --ack H001", "confirm", "godwit confirm does not take --ack"},
		{"rollout is plan only", "godwit apply --rollout direct", "apply", "godwit apply does not take --rollout"},
		{"ack is not a plan flag", "godwit plan --ack H001", "plan", "godwit plan does not take --ack"},
		{
			"ack with no value", "godwit apply --ack", "apply",
			"godwit apply --ack names no hazard code (want --ack H001 or --ack H001,H003)",
		},
		{"bad hazard code", "godwit apply --ack h1", "apply", "godwit apply --ack 'h1' is not a hazard code"},
		{"empty hazard code", "godwit apply --ack=", "apply", "godwit apply --ack '' is not a hazard code"},
		{"hazard code without digits", "godwit revert --ack HHHH", "revert", "godwit revert --ack 'HHHH' is not a hazard code"},
		{"one bad code in a list", "godwit apply --ack H001,x", "apply", "godwit apply --ack 'x' is not a hazard code"},
		{
			"rollout with no value", "godwit plan --rollout", "plan",
			"godwit plan --rollout names no rollout policy (want --rollout direct or --rollout expand-contract)",
		},
		{"unknown rollout", "godwit plan --rollout fast", "plan", "godwit plan --rollout 'fast' is not a rollout policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := comment.Parse(tc.body, tc.want)
			if got != nil || err == nil {
				t.Fatalf("Parse(%q, %q) = %+v, %v; want a refusal", tc.body, tc.want, got, err)
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("Parse(%q, %q) = %q, want it to carry %q", tc.body, tc.want, err, tc.msg)
			}
		})
	}
}

func TestParseUsageNamesTheCommand(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]string{
		"plan":    "'godwit plan --rollout expand-contract'",
		"apply":   "'godwit apply --ack H001,H003'",
		"confirm": "'godwit confirm' or 'godwit confirm <sha>'",
		"revert":  "'godwit revert --allow-data-loss'",
	} {
		_, err := comment.Parse("godwit "+name+" --unknown", name)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s usage = %v, want it to carry %q", name, err, want)
		}
		if strings.Contains(err.Error(), "/godwit") {
			t.Fatalf("%s usage documents the slash alias: %v", name, err)
		}
	}
}
