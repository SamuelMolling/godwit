package controlplane

import (
	"strings"
	"testing"
)

func TestUsingCannotCloseTheTriggerBody(t *testing.T) {
	t.Parallel()
	conn := newScratch(t, usersDDL)

	exp := expandAndApply(t, conn,
		"-- godwit: change-type users.age bigint using='length($godwit$ x $godwit$)::bigint'\n")
	if !strings.Contains(exp.UpSQL, "AS $godwit0$") || strings.Contains(exp.UpSQL, "AS $godwit$") {
		t.Fatalf("the trigger body must not be quoted with a tag the expression carries:\n%s", exp.UpSQL)
	}
}
