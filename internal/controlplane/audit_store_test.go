package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestAuditLog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)

	runID := "99999999-0000-0000-0000-000000000001"
	entries := []AuditEntry{
		{Actor: "samuel", Action: AuditTargetRegister, Target: "app", Detail: "provider=static"},
		{Actor: "ci", Action: AuditRunCreate, RunID: runID, Target: "app", Detail: "rollout=direct"},
		{Actor: "ci", Action: AuditRunCreate, Target: "other"},
	}
	for _, e := range entries {
		if err := s.Audit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.ListAudit(ctx, "", "", 0)
	if err != nil || len(all) != 3 || all[0].Target != "other" || all[2].Actor != "samuel" {
		t.Fatalf("all = %+v, err = %v", all, err)
	}
	if e := all[1]; e.ID <= all[2].ID || time.Since(e.At) > time.Minute || e.RunID != runID || e.Detail != "rollout=direct" {
		t.Fatalf("entry = %+v", e)
	}
	byTarget, err := s.ListAudit(ctx, "app", "", 1)
	if err != nil || len(byTarget) != 1 || byTarget[0].Action != AuditRunCreate {
		t.Fatalf("by target = %+v, err = %v", byTarget, err)
	}
	byRun, err := s.ListAudit(ctx, "", runID, 0)
	if err != nil || len(byRun) != 1 || byRun[0].RunID != runID {
		t.Fatalf("by run = %+v, err = %v", byRun, err)
	}
	if _, err := s.ListAudit(ctx, "", "nope", 0); err == nil || !strings.Contains(err.Error(), "read audit") {
		t.Fatalf("bad run id: %v", err)
	}
	if err := s.Audit(ctx, AuditEntry{Actor: "ci", Action: AuditRunPark, RunID: "nope", Target: "app"}); err == nil ||
		!strings.Contains(err.Error(), "write audit") {
		t.Fatalf("bad run id on write: %v", err)
	}
	s.pool.(interface{ Close() }).Close()
	if _, err := s.ListAudit(ctx, "", "", 0); err == nil || !strings.Contains(err.Error(), "list audit") {
		t.Fatalf("closed pool: %v", err)
	}
}

func TestListAuditRowError(t *testing.T) {
	t.Parallel()
	mock, s := newMockStore(t)

	mock.ExpectQuery("SELECT id, at, actor").WithArgs("", "", 100).
		WillReturnRows(pgxmock.NewRows([]string{"id", "at", "actor", "action", "coalesce", "target", "detail"}).
			AddRow(int64(1), now(), "ci", AuditRunCreate, "", "app", "").RowError(0, errBoom))

	if _, err := s.ListAudit(context.Background(), "", "", 0); err == nil || !strings.Contains(err.Error(), "read audit") {
		t.Fatalf("err = %v", err)
	}
}
