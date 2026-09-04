package ui

import (
	"net/http"
	"strings"
	"testing"
)

func mustOrigins(t *testing.T, raw ...string) []Origin {
	t.Helper()
	out, err := ParseOrigins(raw)
	if err != nil {
		t.Fatalf("ParseOrigins(%v) = %v", raw, err)
	}

	return out
}

func TestParseOriginsRefusesAnythingButSchemeAndHost(t *testing.T) {
	if got := mustOrigins(t, "https://godwit.example.com", " ", "http://127.0.0.1:8474"); len(got) != 2 {
		t.Fatalf("parsed = %v", got)
	}
	for _, bad := range []string{
		"godwit.example.com", "ftp://godwit.example.com", "https://", "https://u:p@godwit.example.com",
		"https://godwit.example.com/ui", "https://godwit.example.com?a=1", "https://godwit.example.com#x", "http://[::1",
	} {
		if _, err := ParseOrigins([]string{bad}); err == nil || !strings.Contains(err.Error(), bad) {
			t.Fatalf("ParseOrigins(%q) = %v", bad, err)
		}
	}
}

func TestPostFromAnotherSiteIsRefused(t *testing.T) {
	h := newUI(fixture(), Config{})
	for _, hdr := range [][]string{
		{"Sec-Fetch-Site", "cross-site"},
		{"Sec-Fetch-Site", "same-site"},
		{"Sec-Fetch-Site", "none"},
		{"Origin", "https://evil.example.com"},
		{"Origin", "null"},
		{"Origin", "http://[::1"},
		{"X-Nothing", "at-all"},
	} {
		rec := do(h, http.MethodPost, "/ui/drift/app/accept", nil, append([]string{"Sec-Fetch-Site", ""}, hdr...)...)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "cross-site request refused") {
			t.Fatalf("%v: code = %d body = %s", hdr, rec.Code, rec.Body.String())
		}
	}
	if h.svc.(*stub).calls != nil {
		t.Fatalf("service was called: %v", h.svc.(*stub).calls)
	}
}

func TestPostWithAMatchingOriginIsAllowed(t *testing.T) {
	h := newUI(fixture(), Config{})
	redirect(t, do(h, http.MethodPost, "/ui/drift/app/accept", nil,
		"Sec-Fetch-Site", "", "Origin", "http://example.com"), "/ui/drift?target=app")
}

func TestConfiguredOriginsGateFormPostsAndHosts(t *testing.T) {
	h := newUI(fixture(), Config{Origins: mustOrigins(t, "https://godwit.example.com")})
	rec := do(h, http.MethodGet, "/ui/", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "unknown host") {
		t.Fatalf("host example.com: code = %d", rec.Code)
	}
	rec = do(h, http.MethodPost, "https://godwit.example.com/ui/drift/app/accept", nil,
		"Sec-Fetch-Site", "", "Origin", "http://example.com")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "cross-site request refused") {
		t.Fatalf("unlisted origin: code = %d", rec.Code)
	}
	redirect(t, do(h, http.MethodPost, "https://GODWIT.example.com/ui/drift/app/accept", nil,
		"Sec-Fetch-Site", "", "Origin", "https://godwit.example.com"), "/ui/drift?target=app")
}

func TestEveryUIResponseCarriesTheSecurityHeaders(t *testing.T) {
	h := newUI(fixture(), Config{})
	for _, path := range []string{"/ui/", "/ui/mark.svg", "/ui/app.js"} {
		hdr := do(h, http.MethodGet, path, nil).Header()
		if !strings.Contains(hdr.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
			hdr.Get("X-Frame-Options") != "DENY" || hdr.Get("X-Content-Type-Options") != "nosniff" ||
			hdr.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: headers = %v", path, hdr)
		}
	}
	if got := do(h, http.MethodGet, "/ui/", nil).Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("page cache-control = %q", got)
	}
	if got := do(h, http.MethodGet, "/ui/app.js", nil).Header().Get("Cache-Control"); got != "public, max-age=86400" {
		t.Fatalf("asset cache-control = %q", got)
	}
}

func TestTheCopyScriptIsServedRatherThanInlined(t *testing.T) {
	h := newUI(fixture(), Config{})
	want(t, do(h, http.MethodGet, "/ui/app.js", nil), http.StatusOK, "navigator.clipboard")
	absent(t, do(h, http.MethodGet, "/ui/diff", nil), "navigator.clipboard")
	want(t, do(h, http.MethodGet, "/ui/diff", nil), http.StatusOK, `<script src="/ui/app.js" defer></script>`)
}
