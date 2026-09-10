package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestADirectoryListingCarriesNamesAndSizes(t *testing.T) {
	t.Parallel()

	var gotPath, gotQuery, gotAccept string
	r := testRepoClient(t, func(w http.ResponseWriter, req *http.Request) {
		gotPath, gotQuery, gotAccept = req.URL.Path, req.URL.RawQuery, req.Header.Get("Accept")
		_, _ = io.WriteString(w, `[{"name":"20260101000000_a.up.sql","type":"file","size":12},
			{"name":"old","type":"dir","size":0}]`)
	})
	got, err := r.directory(context.Background(), "db/migrations", testHead)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/"+testRepo+"/contents/db/migrations" || gotQuery != "ref="+testHead {
		t.Fatalf("path = %s?%s", gotPath, gotQuery)
	}
	if gotAccept != acceptJSON {
		t.Fatalf("accept = %s", gotAccept)
	}
	want := contents{entries: []content{
		{name: "20260101000000_a.up.sql", size: 12, file: true},
		{name: "old", size: 0, file: false},
	}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("contents = %+v", got)
	}
}

func TestADirectoryTheCommitDoesNotCarryIsAbsentRatherThanBroken(t *testing.T) {
	t.Parallel()

	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if _, err := r.directory(context.Background(), "db/migrations", testHead); !errors.Is(err, errAbsent) {
		t.Fatalf("err = %v", err)
	}
}

func TestAPathThatNamesAFileIsNotADirectory(t *testing.T) {
	t.Parallel()

	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"type":"file","name":"godwit.yaml"}`)
	})
	_, err := r.directory(context.Background(), "godwit.yaml", testHead)
	if err == nil || !strings.Contains(err.Error(), "is not a directory at 1111111") {
		t.Fatalf("err = %v", err)
	}
}

func TestADirectoryGitHubRefusesToList(t *testing.T) {
	t.Parallel()

	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if _, err := r.directory(context.Background(), "db/migrations", testHead); err == nil {
		t.Fatal("no error")
	}
}

// The contents API answers a full page for a directory it truncated and says nothing, so a full page
// is the only sign there is.
func TestADirectoryAtTheContentsCapIsReadAsPartial(t *testing.T) {
	t.Parallel()

	entries := make([]contentEntry, contentsCap)
	for i := range entries {
		entries[i] = contentEntry{Name: fmt.Sprintf("%014d_m.up.sql", i), Type: "file", Size: 1}
	}
	body, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) })
	got, err := r.directory(context.Background(), "db/migrations", testHead)
	if err != nil {
		t.Fatal(err)
	}
	if !got.capped {
		t.Fatalf("capped = false at %d entries", len(got.entries))
	}
}

func TestABlobComesBackAsItsOwnBytes(t *testing.T) {
	t.Parallel()

	var gotPath, gotAccept string
	r := testRepoClient(t, func(w http.ResponseWriter, req *http.Request) {
		gotPath, gotAccept = req.URL.Path, req.Header.Get("Accept")
		_, _ = io.WriteString(w, "CREATE TABLE orders ();")
	})
	body, err := r.blob(context.Background(), "db/migrations/20260101000000_a.up.sql", testHead, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "CREATE TABLE orders ();" {
		t.Fatalf("body = %q", body)
	}
	if gotAccept != acceptRaw {
		t.Fatalf("accept = %s", gotAccept)
	}
	if gotPath != "/repos/"+testRepo+"/contents/db/migrations/20260101000000_a.up.sql" {
		t.Fatalf("path = %s", gotPath)
	}
}

// A body read short would be planned as a migration nobody wrote, so the limit refuses rather than trims.
func TestABlobOverTheLimitIsRefusedRatherThanTrimmed(t *testing.T) {
	t.Parallel()

	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 41))
	})
	_, err := r.blob(context.Background(), "db/migrations/a.up.sql", testHead, 40)
	if err == nil || !strings.Contains(err.Error(), "over the 40 bytes") {
		t.Fatalf("err = %v", err)
	}
}

func TestABlobTheCommitDoesNotCarry(t *testing.T) {
	t.Parallel()

	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if _, err := r.blob(context.Background(), "db/migrations/a.up.sql", testHead, 40); !errors.Is(err, errAbsent) {
		t.Fatalf("err = %v", err)
	}
}

func TestABlobGitHubRefuses(t *testing.T) {
	t.Parallel()

	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if _, err := r.blob(context.Background(), "db/migrations/a.up.sql", testHead, 40); err == nil {
		t.Fatal("no error")
	}
}

func TestABlobFromAnUnreachableGitHub(t *testing.T) {
	t.Parallel()

	r := &repoClient{client: &Client{BaseURL: "http://[::1", HTTP: http.DefaultClient}, token: "x", repository: testRepo}
	if _, err := r.blob(context.Background(), "a.sql", testHead, 40); err == nil {
		t.Fatal("no error")
	}
}

func TestACheckOpensInProgressAndIsConcludedOnTheSameRun(t *testing.T) {
	t.Parallel()

	var opened, concluded map[string]any
	var openPath, concludePath, method string
	r := testRepoClient(t, func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if req.Method == http.MethodPost {
			openPath = req.URL.Path
			_ = json.Unmarshal(body, &opened)
			_, _ = io.WriteString(w, `{"id":991}`)

			return
		}
		concludePath, method = req.URL.Path, req.Method
		_ = json.Unmarshal(body, &concluded)
		_, _ = io.WriteString(w, `{}`)
	})
	id, err := r.startCheck(context.Background(), checkRun{
		name: "godwit/plan", head: testHead, title: "planning", summary: "godwit is reading the migrations",
		url: "https://godwit.test/ui/plans/p1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != 991 {
		t.Fatalf("id = %d", id)
	}
	if openPath != "/repos/"+testRepo+"/check-runs" {
		t.Fatalf("path = %s", openPath)
	}
	if opened["status"] != "in_progress" || opened["head_sha"] != testHead || opened["name"] != "godwit/plan" {
		t.Fatalf("opened = %v", opened)
	}
	if opened["started_at"] != now.UTC().Format("2006-01-02T15:04:05Z") {
		t.Fatalf("started_at = %v", opened["started_at"])
	}
	if opened["details_url"] != "https://godwit.test/ui/plans/p1" {
		t.Fatalf("details_url = %v", opened["details_url"])
	}
	if err := r.endCheck(context.Background(), id, checkRun{
		conclusion: "success", title: "nothing to apply", summary: "the target already has them",
	}); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPatch || concludePath != "/repos/"+testRepo+"/check-runs/991" {
		t.Fatalf("%s %s", method, concludePath)
	}
	if concluded["status"] != "completed" || concluded["conclusion"] != "success" {
		t.Fatalf("concluded = %v", concluded)
	}
	if _, ok := concluded["details_url"]; ok {
		t.Fatalf("details_url = %v with no public url", concluded["details_url"])
	}
	out, _ := concluded["output"].(map[string]any)
	if out["title"] != "nothing to apply" {
		t.Fatalf("output = %v", concluded["output"])
	}
}

func TestACheckGitHubRefuses(t *testing.T) {
	t.Parallel()

	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if _, err := r.startCheck(context.Background(), checkRun{name: "godwit/plan", head: testHead}); err == nil {
		t.Fatal("no error on start")
	}
	if err := r.endCheck(context.Background(), 1, checkRun{conclusion: "failure"}); err == nil {
		t.Fatal("no error on end")
	}
}

func TestALongReportIsClippedWhereARuneEnds(t *testing.T) {
	t.Parallel()

	if got := clip("short", 10); got != "short" {
		t.Fatalf("clip = %q", got)
	}
	for _, limit := range []int{8, 9} {
		got := clip(strings.Repeat("é", 10), limit)
		if !strings.HasSuffix(got, ellipsis) || len(got) > limit {
			t.Fatalf("clip = %q (%d bytes, limit %d)", got, len(got), limit)
		}
		if !utf8Valid(got) {
			t.Fatalf("clip split a rune: %q", got)
		}
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}

	return true
}
