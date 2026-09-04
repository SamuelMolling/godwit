package ui

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Origin is a browser-facing origin the UI answers on, as produced by ParseOrigins.
type Origin struct {
	value string
	host  string
}

// ParseOrigins reads the origins /ui is reached at, each scheme://host[:port] and nothing else.
func ParseOrigins(raw []string) ([]Origin, error) {
	var out []Origin
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("ui origin %q: want http://host[:port] or https://host[:port]", s)
		}
		out = append(out, Origin{value: u.Scheme + "://" + u.Host, host: u.Host})
	}

	return out, nil
}

const policy = "default-src 'none'; " +
	"script-src 'self' https://cdnjs.cloudflare.com; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src https://fonts.gstatic.com; " +
	"img-src 'self'; connect-src 'self'; form-action 'self'; " +
	"base-uri 'none'; frame-ancestors 'none'"

func harden(hdr http.Header) {
	hdr.Set("Content-Security-Policy", policy)
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("Cache-Control", "no-store")
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// sameOrigin trusts Sec-Fetch-Site where the browser sends it and falls back to Origin; neither means not a browser.
func (h *Handler) sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return true
	case "":
		return h.knownOrigin(r.Header.Get("Origin"), r.Host)
	default:
		return false
	}
}

func (h *Handler) knownOrigin(origin, host string) bool {
	if origin == "" {
		return false
	}
	if len(h.cfg.Origins) > 0 {
		return matches(origin, func(o Origin) string { return o.value }, h.cfg.Origins)
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}

	return u.Host != "" && strings.EqualFold(u.Host, host)
}

func (h *Handler) knownHost(host string) bool {
	return len(h.cfg.Origins) == 0 || matches(host, func(o Origin) string { return o.host }, h.cfg.Origins)
}

func matches(want string, field func(Origin) string, origins []Origin) bool {
	for _, o := range origins {
		if strings.EqualFold(want, field(o)) {
			return true
		}
	}

	return false
}
