package credstest

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fake struct {
	dsn string
	err error
}

func (f fake) DSN(_ context.Context, config map[string]string) (string, error) {
	if len(config) == 0 {
		return "", errors.New("empty config")
	}

	return f.dsn, f.err
}

type lax struct{}

func (lax) DSN(context.Context, map[string]string) (string, error) { return "always", nil }

func TestCheck(t *testing.T) {
	t.Parallel()

	if p := Check(fake{dsn: "ok"}, map[string]string{"k": "v"}, "ok"); len(p) != 0 {
		t.Fatalf("problems = %v", p)
	}
	p := Check(fake{err: errors.New("boom")}, map[string]string{"k": "v"}, "ok")
	if len(p) != 1 || !strings.Contains(p[0], "boom") {
		t.Fatalf("problems = %v", p)
	}
	p = Check(fake{dsn: "wrong"}, map[string]string{"k": "v"}, "ok")
	if len(p) != 1 || !strings.Contains(p[0], "want") {
		t.Fatalf("problems = %v", p)
	}
	p = Check(lax{}, map[string]string{"k": "v"}, "always")
	if len(p) != 1 || !strings.Contains(p[0], "empty config must fail") {
		t.Fatalf("problems = %v", p)
	}
}

func TestConformancePasses(t *testing.T) {
	t.Parallel()

	Conformance(t, fake{dsn: "ok"}, map[string]string{"k": "v"}, "ok")
}

type recordingTB struct {
	testing.TB
	errors int
}

func (r *recordingTB) Helper()      {}
func (r *recordingTB) Error(...any) { r.errors++ }

func TestConformanceReportsProblems(t *testing.T) {
	t.Parallel()

	rec := &recordingTB{TB: t}
	Conformance(rec, lax{}, map[string]string{"k": "v"}, "always")
	if rec.errors != 1 {
		t.Fatalf("errors = %d, want 1", rec.errors)
	}
}
