package creds

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// ProviderGCPKMS names the Google Cloud KMS key provider.
const ProviderGCPKMS = "gcpkms"

// DefaultGCPKMSEndpoint is the Cloud KMS REST base URL.
const DefaultGCPKMSEndpoint = "https://cloudkms.googleapis.com"

// GCPKMS seals a fresh data key per value with a Cloud KMS key and stores the wrapped data key beside
// the ciphertext: KMS is called to unwrap 32 bytes, and never sees the DSN.
type GCPKMS struct {
	// KeyName is the CryptoKey resource name, projects/…/locations/…/keyRings/…/cryptoKeys/…. Key
	// versions are not named: Cloud KMS picks the primary to encrypt and reads the version out of the
	// ciphertext to decrypt, which is what makes a KMS key rotation invisible here.
	KeyName  string
	Endpoint string
	// Token returns the OAuth2 access token the calls are made with.
	Token  func(ctx context.Context) (string, error)
	Client *http.Client
}

// GCPKMSFromEnv builds the provider from GODWIT_KMS_KEY, GODWIT_KMS_ENDPOINT, GOOGLE_OAUTH_ACCESS_TOKEN
// and GCE_METADATA_HOST.
func GCPKMSFromEnv() (GCPKMS, error) {
	name := os.Getenv("GODWIT_KMS_KEY")
	if name == "" {
		return GCPKMS{}, errors.New("key provider gcpkms needs GODWIT_KMS_KEY, a projects/…/cryptoKeys/… resource name")
	}
	client := http.DefaultClient

	return GCPKMS{
		KeyName:  name,
		Endpoint: cmp.Or(os.Getenv("GODWIT_KMS_ENDPOINT"), DefaultGCPKMSEndpoint),
		Token: MetadataToken(os.Getenv("GOOGLE_OAUTH_ACCESS_TOKEN"),
			cmp.Or(os.Getenv("GCE_METADATA_HOST"), "metadata.google.internal"), client),
		Client: client,
	}, nil
}

// MetadataToken returns a token source: the static token when one is given, the workload's own token
// from the GCE metadata server otherwise.
func MetadataToken(static, host string, client *http.Client) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if static != "" {
			return static, nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"http://"+host+"/computeMetadata/v1/instance/service-account/default/token", nil)
		if err != nil {
			return "", fmt.Errorf("metadata token: %w", err)
		}
		req.Header.Set("Metadata-Flavor", "Google")
		var out struct {
			AccessToken string `json:"access_token"`
		}
		if err := jsonRequest(client, req, &out); err != nil {
			return "", fmt.Errorf("metadata token: %w", err)
		}
		if out.AccessToken == "" {
			return "", errors.New("metadata token: the metadata server returned no access_token")
		}

		return out.AccessToken, nil
	}
}

// Name implements KeyProvider.
func (GCPKMS) Name() string { return ProviderGCPKMS }

// KeyID implements KeyProvider.
func (p GCPKMS) KeyID() string { return p.KeyName }

// Seal implements KeyProvider.
func (p GCPKMS) Seal(ctx context.Context, aad []byte, plaintext string) ([]byte, error) {
	dataKey, err := randomDataKey()
	if err != nil {
		return nil, err
	}
	inner, err := sealGCM(dataKey, aad, plaintext)
	if err != nil {
		return nil, err
	}
	var out struct {
		Ciphertext string `json:"ciphertext"`
	}
	body := map[string]string{
		"plaintext":                   base64.StdEncoding.EncodeToString(dataKey),
		"additionalAuthenticatedData": base64.StdEncoding.EncodeToString(aad),
	}
	if err := p.call(ctx, p.KeyName+":encrypt", body, &out); err != nil {
		return nil, fmt.Errorf("cloud kms encrypt: %w", err)
	}
	wrapped, err := base64.StdEncoding.DecodeString(out.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("cloud kms encrypt: decode ciphertext: %w", err)
	}

	return joinBlob(wrapped, inner)
}

// Open implements KeyProvider. The key the header names is the one asked to unwrap, so a value sealed
// under a key the deployment has since moved off still opens while IAM still grants that key.
func (p GCPKMS) Open(ctx context.Context, aad []byte, keyID string, blob []byte) (string, error) {
	wrapped, inner, err := splitBlob(blob)
	if err != nil {
		return "", err
	}
	var out struct {
		Plaintext string `json:"plaintext"`
	}
	body := map[string]string{
		"ciphertext":                  base64.StdEncoding.EncodeToString(wrapped),
		"additionalAuthenticatedData": base64.StdEncoding.EncodeToString(aad),
	}
	if err := p.call(ctx, cmp.Or(keyID, p.KeyName)+":decrypt", body, &out); err != nil {
		return "", fmt.Errorf("cloud kms decrypt: %w", err)
	}
	dataKey, err := base64.StdEncoding.DecodeString(out.Plaintext)
	if err != nil {
		return "", fmt.Errorf("cloud kms decrypt: decode plaintext: %w", err)
	}

	return openGCM(dataKey, aad, inner)
}

func (p GCPKMS) call(ctx context.Context, resource string, body, out any) error {
	token, err := p.Token(ctx)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(body) // map[string]string cannot fail
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(p.Endpoint, "/")+"/v1/"+resource, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	return jsonRequest(p.Client, req, out)
}

func jsonRequest(client *http.Client, req *http.Request, out any) error {
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
