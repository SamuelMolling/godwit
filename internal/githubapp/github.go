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
)

const (
	defaultAPIBaseURL = "https://api.github.com"
	responseLimit     = 8 << 20
)

type repoView interface {
	permission(ctx context.Context, login string) (string, error)
	pullRequest(ctx context.Context, number int) (pull, error)
	reviews(ctx context.Context, number int) ([]review, error)
}

type forge interface {
	repository(ctx context.Context, installation, repositoryID int64, repository string) (repoView, error)
}

type pull struct {
	head     string
	headRepo string
	state    string
	merged   bool
	author   string
}

type review struct {
	login    string
	state    string
	commitID string
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
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

func (r *repoClient) pullRequest(ctx context.Context, number int) (pull, error) {
	var out pullBody
	if _, err := r.client.call(ctx, http.MethodGet, r.url("/pulls/"+strconv.Itoa(number)), r.token, nil, &out); err != nil {
		return pull{}, err
	}

	return pull{
		head: out.Head.SHA, headRepo: out.Head.Repo.FullName,
		state: out.State, merged: out.Merged, author: out.User.Login,
	}, nil
}

type reviewBody struct {
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (r *repoClient) reviews(ctx context.Context, number int) ([]review, error) {
	url := r.url("/pulls/" + strconv.Itoa(number) + "/reviews?per_page=100")
	var all []review
	for url != "" {
		var page []reviewBody
		next, err := r.client.call(ctx, http.MethodGet, url, r.token, nil, &page)
		if err != nil {
			return nil, err
		}
		for _, p := range page {
			all = append(all, review{login: p.User.Login, state: p.State, commitID: p.CommitID})
		}
		url = next
	}

	return all, nil
}

func (r *repoClient) url(path string) string {
	return r.client.base() + "/repos/" + r.repository + path
}

func (c *Client) call(ctx context.Context, method, url, token string, body []byte, out any) (string, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit))
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", method, url, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%s %s: %w", method, url, errNotFound)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("%s %s: github answered %d", method, url, resp.StatusCode)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return "", fmt.Errorf("%s %s: %w", method, url, err)
	}

	return nextPage(resp.Header.Get("Link")), nil
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
