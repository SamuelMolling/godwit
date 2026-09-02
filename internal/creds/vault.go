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

// VaultFromEnv builds a Vault provider from VAULT_ADDR, VAULT_TOKEN, VAULT_K8S_ROLE, VAULT_K8S_MOUNT and VAULT_K8S_JWT.
func VaultFromEnv() Vault {
	return Vault{
		Address: os.Getenv("VAULT_ADDR"),
		Token:   os.Getenv("VAULT_TOKEN"),
		Role:    os.Getenv("VAULT_K8S_ROLE"),
		Mount:   cmp.Or(os.Getenv("VAULT_K8S_MOUNT"), "kubernetes"),
		JWTPath: cmp.Or(os.Getenv("VAULT_K8S_JWT"), "/var/run/secrets/kubernetes.io/serviceaccount/token"),
		Client:  http.DefaultClient,
	}
}

// DSN implements Provider.
func (p Vault) DSN(ctx context.Context, config map[string]string) (string, error) {
	path, ok := config["path"]
	if !ok {
		return "", errors.New(`vault target config missing "path"`)
	}
	if p.Address == "" {
		return "", errors.New("vault provider not configured: set VAULT_ADDR")
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

	return render(config["template"], secret.Data)
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

func render(template string, data map[string]any) (string, error) {
	if template == "" {
		template = "{{dsn}}"
	}
	out := template
	for k, v := range data {
		if s, ok := v.(string); ok {
			out = strings.ReplaceAll(out, "{{"+k+"}}", s)
		}
	}
	if i := strings.Index(out, "{{"); i >= 0 {
		return "", fmt.Errorf("vault secret has no field for %s", out[i:])
	}

	return out, nil
}
