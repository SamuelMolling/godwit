// Package comment parses a pull request comment into the godwit command it names.
package comment

import (
	"fmt"
	"strings"
)

// Command is the command a comment named and the flags that command carried.
type Command struct {
	Name          string
	Sha           string
	Ack           []string
	AllowDataLoss bool
	Force         bool
	Rollout       string
}

type grammar struct {
	flags map[string]bool
	usage string
}

var grammars = map[string]grammar{
	"plan": {
		flags: map[string]bool{"--rollout": true},
		usage: "'godwit plan', 'godwit plan <sha>' or 'godwit plan --rollout expand-contract'",
	},
	"apply": {
		flags: map[string]bool{"--ack": true},
		usage: "'godwit apply', 'godwit apply <sha>' or 'godwit apply --ack H001,H003'",
	},
	"confirm": {
		flags: map[string]bool{},
		usage: "'godwit confirm' or 'godwit confirm <sha>'",
	},
	"revert": {
		flags: map[string]bool{"--ack": true, "--allow-data-loss": true, "--force": true},
		usage: "'godwit revert', 'godwit revert --ack H001', 'godwit revert --allow-data-loss' or 'godwit revert --force'",
	},
}

type valued struct{ noun, want string }

var valuedFlags = map[string]valued{
	"--ack":     {"hazard code", "want --ack H001 or --ack H001,H003"},
	"--rollout": {"rollout policy", "want --rollout direct or --rollout expand-contract"},
}

var known = func() map[string]bool {
	all := map[string]bool{}
	for _, g := range grammars {
		for flag := range g.flags {
			all[flag] = true
		}
	}

	return all
}()

// Parse returns the first godwit command a whole line of body names outside a fenced code block, or nil for none; want narrows to one command, "" accepts any.
func Parse(body, want string) (*Command, error) {
	if _, ok := grammars[want]; want != "" && !ok {
		return nil, fmt.Errorf("'%s' is not a comment command (want plan, apply, confirm or revert)", want)
	}
	fenced := false
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fenced = !fenced

			continue
		}
		if fenced {
			continue
		}
		words := invocation(line)
		if len(words) == 0 {
			continue
		}
		if _, ok := grammars[words[0]]; !ok {
			continue
		}
		if want != "" && words[0] != want {
			continue
		}

		return arguments(words[0], words[1:])
	}

	return nil, nil
}

func invocation(line string) []string {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(line, "/"), "godwit")
	if !ok || (rest != "" && strings.TrimLeft(rest, " \t") == rest) {
		return nil
	}

	return strings.Fields(rest)
}

func arguments(name string, args []string) (*Command, error) {
	g := grammars[name]
	cmd := &Command{Name: name}
	if len(args) > 0 && isSha(args[0]) {
		cmd.Sha = args[0]
		args = args[1:]
	}
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		v, takesValue := valuedFlags[flag]
		if !known[flag] || (assigned && !takesValue) {
			return nil, fmt.Errorf("godwit %s does not understand '%s' (want %s)", name, args[i], g.usage)
		}
		if !g.flags[flag] {
			return nil, fmt.Errorf("godwit %s does not take %s (want %s)", name, flag, g.usage)
		}
		if takesValue && !assigned {
			i++
			if i == len(args) {
				return nil, fmt.Errorf("godwit %s %s names no %s (%s)", name, flag, v.noun, v.want)
			}
			value = args[i]
		}
		if err := set(cmd, flag, value, v); err != nil {
			return nil, err
		}
	}

	return cmd, nil
}

func set(cmd *Command, flag, value string, v valued) error {
	switch flag {
	case "--ack":
		codes := strings.Split(value, ",")
		for _, code := range codes {
			if !isHazard(code) {
				return fmt.Errorf("godwit %s --ack '%s' is not a %s (%s)", cmd.Name, code, v.noun, v.want)
			}
		}
		cmd.Ack = append(cmd.Ack, codes...)
	case "--rollout":
		if value != "direct" && value != "expand-contract" {
			return fmt.Errorf("godwit %s --rollout '%s' is not a %s (%s)", cmd.Name, value, v.noun, v.want)
		}
		cmd.Rollout = value
	case "--allow-data-loss":
		cmd.AllowDataLoss = true
	case "--force":
		cmd.Force = true
	}

	return nil
}

func isSha(word string) bool {
	if len(word) < 7 || len(word) > 40 {
		return false
	}

	return strings.Trim(word, "0123456789abcdef") == ""
}

func isHazard(code string) bool {
	if len(code) != 4 || code[0] < 'A' || code[0] > 'Z' {
		return false
	}

	return strings.Trim(code[1:], "0123456789") == ""
}
