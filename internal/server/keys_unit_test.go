package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/creds"
)

type fakeTargets struct {
	summaries  []controlplane.TargetSummary
	configs    map[string]map[string]string
	listErr    error
	targetErr  error
	registered map[string]map[string]string
	writeErr   error
}

func (f *fakeTargets) ListTargets(context.Context, time.Time) ([]controlplane.TargetSummary, error) {
	return f.summaries, f.listErr
}

func (f *fakeTargets) Target(_ context.Context, name string) (string, map[string]string, error) {
	if f.targetErr != nil {
		return "", nil, f.targetErr
	}

	return "static", f.configs[name], nil
}

func (f *fakeTargets) RegisterTarget(_ context.Context, name, _ string, config map[string]string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.registered == nil {
		f.registered = map[string]map[string]string{}
	}
	f.registered[name] = config

	return nil
}

func capturingLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer

	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func envRing(k []byte, previous ...[]byte) creds.Keyring {
	return creds.NewKeyring(creds.NewEnv(k, previous...))
}

func TestSettleKeysNamesStrandedTargets(t *testing.T) {
	t.Parallel()

	log, buf := capturingLog()
	store := &fakeTargets{summaries: []controlplane.TargetSummary{
		{Name: "app", Provider: "static"},
		{Name: "jobs", Provider: "vault"},
	}}
	settleKeys(context.Background(), store, creds.Keyring{}, log)
	if !strings.Contains(buf.String(), `"targets":"app"`) {
		t.Fatalf("log = %s", buf.String())
	}
	if strings.Contains(buf.String(), "jobs") {
		t.Fatal("a vault target holds no sealed secret")
	}
}

func TestSettleKeysListFailure(t *testing.T) {
	t.Parallel()

	log, buf := capturingLog()
	settleKeys(context.Background(), &fakeTargets{listErr: errors.New("store down")}, creds.Keyring{}, log)
	if !strings.Contains(buf.String(), "not checked") {
		t.Fatalf("log = %s", buf.String())
	}
}

func TestSettleKeysReseals(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	old := bytes.Repeat([]byte("o"), 32)
	sealed, err := envRing(old).Seal(ctx, "postgres://app")
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeTargets{
		summaries: []controlplane.TargetSummary{{Name: "app", Provider: "static"}},
		configs:   map[string]map[string]string{"app": {"dsn": sealed}},
	}
	ring := envRing(testKey, old)
	log, buf := capturingLog()
	settleKeys(ctx, store, ring, log)
	if !strings.Contains(buf.String(), "resealed") {
		t.Fatalf("log = %s", buf.String())
	}
	got := store.registered["app"]["dsn"]
	if ring.NeedsReseal(got) {
		t.Fatalf("still on the old key: %q", got)
	}
	dsn, err := ring.Open(ctx, got)
	if err != nil || dsn != "postgres://app" {
		t.Fatalf("dsn = %q, err = %v", dsn, err)
	}

	store.registered = nil
	settleKeys(ctx, &fakeTargets{
		summaries: store.summaries,
		configs:   map[string]map[string]string{"app": {"dsn": got}},
	}, ring, log)
	if store.registered != nil {
		t.Fatal("a target already on the key must not be rewritten")
	}
}

func TestResealFailures(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	old := bytes.Repeat([]byte("o"), 32)
	sealed, err := envRing(old).Seal(ctx, "postgres://app")
	if err != nil {
		t.Fatal(err)
	}
	summaries := []controlplane.TargetSummary{{Name: "app", Provider: "static"}}
	configs := func() map[string]map[string]string { return map[string]map[string]string{"app": {"dsn": sealed}} }

	for _, tc := range []struct {
		name  string
		store *fakeTargets
		ring  creds.Keyring
		want  string
	}{
		{
			"target unreadable",
			&fakeTargets{summaries: summaries, targetErr: errors.New("gone")},
			envRing(testKey, old), "not read",
		},
		{
			"key gone",
			&fakeTargets{summaries: summaries, configs: configs()},
			envRing(testKey), "sealed under another key",
		},
		{
			"seal fails",
			&fakeTargets{summaries: summaries, configs: configs()},
			creds.NewKeyring(sealOnlyFails{inner: creds.NewEnv(testKey, old)}), "not resealed",
		},
		{
			"write fails",
			&fakeTargets{summaries: summaries, configs: configs(), writeErr: errors.New("read only")},
			envRing(testKey, old), "not resealed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log, buf := capturingLog()
			settleKeys(ctx, tc.store, tc.ring, log)
			if !strings.Contains(buf.String(), tc.want) || !strings.Contains(buf.String(), `"level":"WARN"`) {
				t.Fatalf("log = %s", buf.String())
			}
		})
	}
}

type sealOnlyFails struct{ inner creds.Env }

func (sealOnlyFails) Name() string    { return "env" }
func (s sealOnlyFails) KeyID() string { return s.inner.KeyID() }

func (sealOnlyFails) Seal(context.Context, []byte, string) ([]byte, error) {
	return nil, errors.New("kms down")
}

func (s sealOnlyFails) Open(ctx context.Context, aad []byte, keyID string, blob []byte) (string, error) {
	return s.inner.Open(ctx, aad, keyID, blob)
}
