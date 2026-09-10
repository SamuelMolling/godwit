package githubapp

import (
	"fmt"
	"path"
	"strings"
)

func validGlob(pattern string) error {
	switch {
	case pattern == "":
		return fmt.Errorf("when_modified carries an empty pattern")
	case strings.HasPrefix(pattern, "/"):
		return fmt.Errorf("when_modified pattern %q is absolute; patterns are relative to the project", pattern)
	}
	for _, segment := range strings.Split(pattern, "/") {
		if segment == ".." {
			return fmt.Errorf("when_modified pattern %q leaves the project directory", pattern)
		}
		if _, err := path.Match(strings.ReplaceAll(segment, "**", "*"), ""); err != nil {
			return fmt.Errorf("when_modified pattern %q is malformed: %w", pattern, err)
		}
	}

	return nil
}

func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			return matchStar(pattern[1:], name)
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], name[0]); err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}

	return len(name) == 0
}

func matchStar(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) > 0
	}
	for i := range len(name) + 1 {
		if matchSegments(pattern, name[i:]) {
			return true
		}
	}

	return false
}
