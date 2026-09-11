package githubapp

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SamuelMolling/godwit/internal/authz"
)

const (
	defaultAPIBaseURL = "https://api.github.com"
	responseLimit     = 8 << 20
	// GitHub answers at most 3000 files for a pull request and says nothing when it truncates.
	filesCap   = 3000
	reviewsCap = 3000
	// The contents API answers at most 1000 entries for a directory and says nothing when it truncates.
	contentsCap = 1000
)

type repoView interface {
	react(ctx context.Context, comment int64, reaction string) error
	speak(ctx context.Context, number int, marker, body string) error
	check(ctx context.Context, name, head, title, summary string) error
	startCheck(ctx context.Context, c checkRun) (int64, error)
	endCheck(ctx context.Context, id int64, c checkRun) error
	permission(ctx context.Context, login string) (string, error)
	pullRequest(ctx context.Context, number int) (authz.PullRequest, error)
	reviews(ctx context.Context, number int) ([]authz.Review, bool, error)
	changed(ctx context.Context, number int) (listing, error)
	file(ctx context.Context, path, ref string) ([]byte, error)
	directory(ctx context.Context, path, ref string) (contents, error)
	blob(ctx context.Context, path, ref string, limit int) ([]byte, error)
}

type forge interface {
	repository(ctx context.Context, installation, repositoryID int64, repository string) (repoView, error)
}

// Client authenticates as the App: a JWT to mint an installation token, and that token for everything else.
type Client struct {
	BaseURL string
	AppID   string
	Signer  crypto.Signer
	HTTP    *http.Client
	Now     func() time.Time
}

func (c *Client) base() string { return cmp.Or(c.BaseURL, defaultAPIBaseURL) }

func (c *Client) repository(ctx context.Context, installation, repositoryID int64, repository string) (repoView, error) {
	jwt, err := appJWT(c.AppID, c.Signer, c.Now())
	if err != nil {
		return nil, err
	}
	var out struct {
		Token string `json:"token"`
	}
	url := c.base() + "/app/installations/" + strconv.FormatInt(installation, 10) + "/access_tokens"
	body := fmt.Sprintf(`{"repository_ids":[%d]}`, repositoryID)
	if _, err := c.call(ctx, http.MethodPost, url, jwt, []byte(body), &out); err != nil {
		return nil, fmt.Errorf("mint installation token: %w", err)
	}
	if out.Token == "" {
		return nil, errors.New("mint installation token: github answered with no token")
	}

	return &repoClient{client: c, token: out.Token, repository: repository}, nil
}

type repoClient struct {
	client     *Client
	token      string
	repository string
}

var errNotFound = errors.New("not found")

func (r *repoClient) permission(ctx context.Context, login string) (string, error) {
	var out struct {
		Permission string `json:"permission"`
	}
	url := r.url("/collaborators/" + login + "/permission")
	_, err := r.client.call(ctx, http.MethodGet, url, r.token, nil, &out)
	if errors.Is(err, errNotFound) {
		return "none", nil
	}
	if err != nil {
		return "", err
	}

	return out.Permission, nil
}

type pullBody struct {
	State        string `json:"state"`
	Merged       bool   `json:"merged"`
	ChangedFiles int    `json:"changed_files"`
	Head         struct {
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

func (r *repoClient) pullRequest(ctx context.Context, number int) (authz.PullRequest, error) {
	var out pullBody
	if _, err := r.client.call(ctx, http.MethodGet, r.url("/pulls/"+strconv.Itoa(number)), r.token, nil, &out); err != nil {
		return authz.PullRequest{}, err
	}

	return authz.PullRequest{
		Head: out.Head.SHA, HeadRepo: out.Head.Repo.FullName,
		State: out.State, Merged: out.Merged, Files: out.ChangedFiles,
	}, nil
}

type reviewBody struct {
	State string `json:"state"`
	User  struct {
		Login string `json:"login"`
	} `json:"user"`
}

// reviews reports whether it read all of them: GitHub lists oldest first, so a short read drops the dismissals.
func (r *repoClient) reviews(ctx context.Context, number int) ([]authz.Review, bool, error) {
	url := r.url("/pulls/" + strconv.Itoa(number) + "/reviews?per_page=100")
	var all []authz.Review
	for url != "" {
		var page []reviewBody
		next, err := r.client.call(ctx, http.MethodGet, url, r.token, nil, &page)
		if err != nil {
			return nil, false, err
		}
		for _, p := range page {
			all = append(all, authz.Review{Login: p.User.Login, State: p.State})
		}
		if len(all) >= reviewsCap {
			return nil, false, nil
		}
		url = next
	}

	return all, true, nil
}

type changedFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
}

type listing struct {
	paths  []string
	listed int
	capped bool
}

func (l listing) whole(files int) bool {
	return !l.capped && (files <= 0 || l.listed >= files)
}

func (r *repoClient) changed(ctx context.Context, number int) (listing, error) {
	url := r.url("/pulls/" + strconv.Itoa(number) + "/files?per_page=100")
	var out listing
	for url != "" {
		var page []changedFile
		next, err := r.client.call(ctx, http.MethodGet, url, r.token, nil, &page)
		if err != nil {
			return listing{}, err
		}
		for _, f := range page {
			out.listed++
			out.paths = append(out.paths, f.Filename)
			if f.PreviousFilename != "" {
				out.paths = append(out.paths, f.PreviousFilename)
			}
		}
		if out.listed >= filesCap {
			out.capped = true

			return out, nil
		}
		url = next
	}

	return out, nil
}

var errAbsent = errors.New("absent")

const maxConfigBytes = 64 << 10

func (r *repoClient) file(ctx context.Context, path, ref string) ([]byte, error) {
	var out struct {
		Type     string `json:"type"`
		Size     int    `json:"size"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	url := r.url("/contents/" + path + "?ref=" + ref)
	if _, err := r.client.call(ctx, http.MethodGet, url, r.token, nil, &out); errors.Is(err, errNotFound) {
		return nil, errAbsent
	} else if err != nil {
		return nil, err
	}
	if out.Type != "file" {
		return nil, fmt.Errorf("%s is a %s, not a file", path, orNone(out.Type))
	}
	if out.Size > maxConfigBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d a project file may be", path, out.Size, maxConfigBytes)
	}
	if out.Encoding != "base64" {
		return nil, fmt.Errorf("%s came back %s-encoded", path, orNone(out.Encoding))
	}
	body, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return body, nil
}

type content struct {
	name string
	size int
	file bool
}

type contents struct {
	entries []content
	capped  bool
}

type contentEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int    `json:"size"`
}

func (r *repoClient) directory(ctx context.Context, path, ref string) (contents, error) {
	var raw json.RawMessage
	_, err := r.client.call(ctx, http.MethodGet, r.url("/contents/"+path+"?ref="+ref), r.token, nil, &raw)
	if errors.Is(err, errNotFound) {
		return contents{}, errAbsent
	}
	if err != nil {
		return contents{}, err
	}
	var listed []contentEntry
	if err := json.Unmarshal(raw, &listed); err != nil {
		return contents{}, fmt.Errorf("%s is not a directory at %s", path, short(ref))
	}
	out := contents{capped: len(listed) >= contentsCap, entries: make([]content, 0, len(listed))}
	for _, e := range listed {
		out.entries = append(out.entries, content{name: e.Name, size: e.Size, file: e.Type == "file"})
	}

	return out, nil
}

func (r *repoClient) blob(ctx context.Context, path, ref string, limit int) ([]byte, error) {
	body, err := r.client.fetch(ctx, r.url("/contents/"+path+"?ref="+ref), r.token, limit)
	if errors.Is(err, errNotFound) {
		return nil, errAbsent
	}

	return body, err
}

func (r *repoClient) react(ctx context.Context, comment int64, reaction string) error {
	url := r.url("/issues/comments/" + strconv.FormatInt(comment, 10) + "/reactions")
	body := fmt.Sprintf(`{"content":%q}`, reaction)
	var out struct{}
	_, err := r.client.call(ctx, http.MethodPost, url, r.token, []byte(body), &out)

	return err
}

type issueComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User user   `json:"user"`
}

const commentsCap = 1000

func (r *repoClient) speak(ctx context.Context, number int, marker, body string) error {
	old, err := r.mine(ctx, number, marker)
	if err != nil {
		return err
	}
	for _, id := range old {
		if err := r.drop(ctx, id); err != nil {
			return err
		}
	}

	return r.write(ctx, http.MethodPost, r.url("/issues/"+strconv.Itoa(number)+"/comments"), marker+"\n"+body)
}

func (r *repoClient) drop(ctx context.Context, id int64) error {
	var out struct{}
	_, err := r.client.call(ctx, http.MethodDelete, r.url("/issues/comments/"+strconv.FormatInt(id, 10)), r.token, nil, &out)

	return err
}

func (r *repoClient) check(ctx context.Context, name, head, title, summary string) error {
	body, _ := json.Marshal(map[string]any{ // only strings and one nested map; cannot fail
		"name": name, "head_sha": head, "status": "completed", "conclusion": "failure",
		"output": map[string]string{"title": title, "summary": summary},
	})
	var out struct{}
	_, err := r.client.call(ctx, http.MethodPost, r.url("/check-runs"), r.token, body, &out)

	return err
}

type checkRun struct {
	name       string
	head       string
	title      string
	summary    string
	url        string
	conclusion string
}

const (
	checkTitleMax   = 255
	checkSummaryMax = 65000
	ellipsis        = "…"
)

func (c checkRun) fields() map[string]any {
	out := map[string]any{"output": map[string]string{
		"title": clip(c.title, checkTitleMax), "summary": clip(c.summary, checkSummaryMax),
	}}
	if c.url != "" {
		out["details_url"] = c.url
	}

	return out
}

func (r *repoClient) startCheck(ctx context.Context, c checkRun) (int64, error) {
	fields := c.fields()
	fields["name"], fields["head_sha"] = c.name, c.head
	fields["status"], fields["started_at"] = "in_progress", r.stamp()
	body, _ := json.Marshal(fields) // strings and one nested map; cannot fail
	var out struct {
		ID int64 `json:"id"`
	}
	if _, err := r.client.call(ctx, http.MethodPost, r.url("/check-runs"), r.token, body, &out); err != nil {
		return 0, err
	}

	return out.ID, nil
}

func (r *repoClient) endCheck(ctx context.Context, id int64, c checkRun) error {
	fields := c.fields()
	fields["status"], fields["conclusion"], fields["completed_at"] = "completed", c.conclusion, r.stamp()
	body, _ := json.Marshal(fields) // strings and one nested map; cannot fail
	var out struct{}
	_, err := r.client.call(ctx, http.MethodPatch,
		r.url("/check-runs/"+strconv.FormatInt(id, 10)), r.token, body, &out)

	return err
}

func (r *repoClient) stamp() string {
	return r.client.Now().UTC().Format(time.RFC3339)
}

func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut] + ellipsis
}

func (r *repoClient) write(ctx context.Context, method, url, body string) error {
	payload, _ := json.Marshal(map[string]string{"body": body}) // map[string]string cannot fail
	var out struct{}
	_, err := r.client.call(ctx, method, url, r.token, payload, &out)

	return err
}

func (r *repoClient) mine(ctx context.Context, number int, marker string) ([]int64, error) {
	url := r.url("/issues/" + strconv.Itoa(number) + "/comments?per_page=100")
	var out []int64
	read := 0
	for url != "" {
		var page []issueComment
		next, err := r.client.call(ctx, http.MethodGet, url, r.token, nil, &page)
		if err != nil {
			return nil, err
		}
		for _, c := range page {
			if strings.HasPrefix(c.Body, marker) {
				out = append(out, c.ID)
			}
		}
		read += len(page)
		if read >= commentsCap {
			return out, nil
		}
		url = next
	}

	return out, nil
}

func (r *repoClient) url(path string) string {
	return r.client.base() + "/repos/" + r.repository + path
}

const (
	acceptJSON = "application/vnd.github+json"
	acceptRaw  = "application/vnd.github.raw"
)

func (c *Client) send(ctx context.Context, method, url, token, accept string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	return c.HTTP.Do(req)
}

func answer(resp *http.Response, method, url string, limit int) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s %s: %w", method, url, errNotFound)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: github answered %d", method, url, resp.StatusCode)
	}

	return payload, nil
}

func (c *Client) call(ctx context.Context, method, url, token string, body []byte, out any) (string, error) {
	resp, err := c.send(ctx, method, url, token, acceptJSON, body)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := answer(resp, method, url, responseLimit)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return "", fmt.Errorf("%s %s: %w", method, url, err)
	}

	return nextPage(resp.Header.Get("Link")), nil
}

// fetch refuses a blob over limit rather than truncating it into a migration godwit would then plan.
func (c *Client) fetch(ctx context.Context, url, token string, limit int) ([]byte, error) {
	resp, err := c.send(ctx, http.MethodGet, url, token, acceptRaw, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := answer(resp, http.MethodGet, url, limit+1)
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, fmt.Errorf("GET %s: over the %d bytes a migration file may be", url, limit)
	}

	return body, nil
}

func nextPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		url, rel, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(rel, `rel="next"`) {
			continue
		}
		if url = strings.TrimSpace(url); strings.HasPrefix(url, "<") && strings.HasSuffix(url, ">") {
			return url[1 : len(url)-1]
		}
	}

	return ""
}

const jwtHeader = `{"alg":"RS256","typ":"JWT"}`

func appJWT(appID string, signer crypto.Signer, now time.Time) (string, error) {
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":%q}`, now.Add(-time.Minute).Unix(), now.Add(9*time.Minute).Unix(), appID)
	enc := base64.RawURLEncoding
	signed := enc.EncodeToString([]byte(jwtHeader)) + "." + enc.EncodeToString([]byte(claims))
	digest := sha256.Sum256([]byte(signed))
	signature, err := signer.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}

	return signed + "." + enc.EncodeToString(signature), nil
}
