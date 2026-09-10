package creds

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// Vault reads the DSN from a Vault secret, authenticating with a token or the Kubernetes auth method.
type Vault struct {
	Address string
	Token   string
	Role    string
	Mount   string
	JWTPath string
	Client  *http.Client
}

const defaultJWTPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Target config keys the providers read.
const (
	DSNKey         = "dsn"
	PathKey        = "path"
	TemplateKey    = "template"
	StoreConfigKey = "credential_store"
)

// VaultStore is where a named credential store points: one Vault, and how godwit authenticates there.
type VaultStore struct {
	Address  string
	Role     string
	Mount    string
	TokenEnv string
}

// VaultAudience is what every Vault Kubernetes auth role godwit logs in at must require: a deployment
// is one identity, so this is a constant rather than a property of a store.
const VaultAudience = "godwit"

// VaultTokenPath is where the deployment projects the token minted for VaultAudience.
const VaultTokenPath = "/var/run/secrets/godwit/vault/token"

type vaults struct {
	client *http.Client
	lookup func(ctx context.Context, name string) (VaultStore, error)
	path   string
}

var errNoStore = errors.New("this target names no credential store, and godwit reads no Vault without one: " +
	"register the Vault its credentials live in with `godwit credential-store add <store> --vault-addr=... --vault-k8s-role=...`, " +
	"then point the target at it with `godwit target add <target> --provider=vault --vault-path=... --credential-store=<store>`")

// DSN implements Provider.
func (p vaults) DSN(ctx context.Context, config map[string]string) (string, error) {
	name := config[StoreConfigKey]
	if name == "" {
		return "", errNoStore
	}
	if p.lookup == nil {
		return "", fmt.Errorf("credential store %q: this service resolves no stores", name)
	}
	store, err := p.lookup(ctx, name)
	if err != nil {
		return "", fmt.Errorf("credential store %q: %w", name, err)
	}
	v := Vault{
		Address: store.Address, Role: store.Role, Mount: cmp.Or(store.Mount, "kubernetes"),
		JWTPath: cmp.Or(p.path, VaultTokenPath), Client: p.client,
	}
	if store.TokenEnv != "" {
		if v.Token = os.Getenv(store.TokenEnv); v.Token == "" {
			return "", fmt.Errorf("credential store %q reads its token from %s, and this process has no such value",
				name, store.TokenEnv)
		}
	}

	return v.DSN(ctx, config)
}

// DSN implements Provider.
func (p Vault) DSN(ctx context.Context, config map[string]string) (string, error) {
	path, ok := config[PathKey]
	if !ok {
		return "", errors.New(`vault target config missing "path"`)
	}
	if p.Address == "" {
		return "", errors.New("this vault has no address")
	}
	token, err := p.token(ctx)
	if err != nil {
		return "", err
	}
	var secret struct {
		Data map[string]any `json:"data"`
	}
	if err := p.call(ctx, http.MethodGet, path, token, nil, &secret); err != nil {
		return "", fmt.Errorf("read vault secret %s: %w", path, err)
	}
	if inner, ok := secret.Data["data"].(map[string]any); ok {
		secret.Data = inner
	}

	return render(config[TemplateKey], secret.Data)
}

func (p Vault) token(ctx context.Context) (string, error) {
	if p.Token != "" {
		return p.Token, nil
	}
	jwt, err := os.ReadFile(p.JWTPath)
	if err != nil {
		return "", fmt.Errorf("read service account token: %w", err)
	}
	var login struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	body := map[string]string{"jwt": strings.TrimSpace(string(jwt)), "role": p.Role}
	if err := p.call(ctx, http.MethodPost, "auth/"+p.Mount+"/login", "", body, &login); err != nil {
		return "", fmt.Errorf("vault kubernetes login: %w", err)
	}

	return login.Auth.ClientToken, nil
}

func (p Vault) call(ctx context.Context, method, path, token string, body, out any) error {
	var payload io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.Address, "/")+"/v1/"+path, payload)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

var marker = regexp.MustCompile(`{{([^{}]*)}}`)

// render substitutes the secret's fields into the template in one pass, so a value that itself contains
// a marker is never re-substituted, and names only the template's own keys on failure — the error travels
// to the caller, to cp_runs.error and to notifications, and must carry nothing the secret held.
func render(template string, data map[string]any) (string, error) {
	if template == "" {
		template = "{{dsn}}"
	}
	var missing []string
	out := marker.ReplaceAllStringFunc(template, func(m string) string {
		key := m[2 : len(m)-2]
		if s, ok := data[key].(string); ok {
			return s
		}
		missing = append(missing, key)

		return m
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("vault secret has no field for %s", strings.Join(missing, ", "))
	}
	if strings.Contains(marker.ReplaceAllLiteralString(template, ""), "{{") {
		return "", errors.New("vault template has an unclosed {{ marker")
	}

	return out, nil
}
