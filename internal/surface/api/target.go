package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/creds"
)

// RegisterTarget registers a target and changes a registered one, keeping every setting the request leaves out.
func (s *Server) RegisterTarget(ctx context.Context, req *connect.Request[godwitv1.RegisterTargetRequest]) (*connect.Response[godwitv1.RegisterTargetResponse], error) {
	m := req.Msg
	if m.Name == "" {
		return nil, invalid("name is required")
	}
	sealed, err := s.sealDSN(ctx, m.Dsn)
	if err != nil {
		return nil, err
	}
	var reg controlplane.TargetRegistration
	if err := s.store.Transact(ctx, func(tx *controlplane.Store) error {
		provider, config, err := tx.LockTarget(ctx, m.Name)
		if err != nil {
			return rpcErr(err)
		}
		if provider, err = s.applyTarget(ctx, m, provider, config, sealed); err != nil {
			return err
		}
		reg = controlplane.RegistrationOf(m.Name, provider, config)
		if err := tx.RegisterTarget(ctx, m.Name, provider, config); err != nil {
			return rpcErr(err)
		}

		return nil
	}); err != nil {
		return nil, err
	}
	s.Log.Info("target registered", append([]any{"target", reg.Name}, reg.Fields()...)...)
	s.audit(ctx, controlplane.AuditTargetRegister, "", reg.Name, auditDetail(reg))

	return connect.NewResponse(&godwitv1.RegisterTargetResponse{}), nil
}

// GetTarget returns a target's registration, never its credential.
func (s *Server) GetTarget(ctx context.Context, req *connect.Request[godwitv1.GetTargetRequest]) (*connect.Response[godwitv1.GetTargetResponse], error) {
	if req.Msg.Name == "" {
		return nil, invalid("name is required")
	}
	provider, config, err := s.store.Target(ctx, req.Msg.Name)
	if err != nil {
		return nil, rpcErr(err)
	}
	r := controlplane.RegistrationOf(req.Msg.Name, provider, config)

	return connect.NewResponse(&godwitv1.GetTargetResponse{
		Name: r.Name, Provider: r.Provider, CredentialStore: r.CredentialStore,
		VaultPath: r.VaultPath, VaultTemplate: r.VaultTemplate, SecretPath: r.SecretPath,
		DsnRegistered: r.DSNRegistered,
		LockTimeout:   r.Timeouts.Lock, StatementTimeout: r.Timeouts.Statement,
		RequirePlan: r.RequirePlan, KeepOld: r.KeepOld, IgnoreAdoptedTables: r.IgnoreAdoptedTables,
		SearchPath: r.SearchPath, GithubRepositories: r.GitHubRepositories,
	}), nil
}

func (s *Server) sealDSN(ctx context.Context, dsn string) (string, error) {
	if dsn == "" {
		return "", nil
	}
	if !s.keys.Configured() {
		return "", invalid("static provider needs a key: set GODWIT_MASTER_KEY, or GODWIT_KEY_PROVIDER with GODWIT_KMS_KEY")
	}
	enc, err := s.keys.Seal(ctx, dsn)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}

	return enc, nil
}

func (s *Server) applyTarget(ctx context.Context, m *godwitv1.RegisterTargetRequest, provider string, config map[string]string, sealed string) (string, error) {
	if m.Provider != "" && m.Provider != provider {
		provider = m.Provider
		maps.DeleteFunc(config, func(k, _ string) bool { return credentialKeys[k] })
	}
	switch {
	case provider == "":
		return "", invalid(fmt.Sprintf("target %q is not registered; a new one needs a provider: static, kubernetes or vault", m.Name))
	case !providers[provider]:
		return "", invalid("unknown provider " + provider)
	}
	if err := misplaced(m, provider); err != nil {
		return "", err
	}
	if err := s.credentials(ctx, m, provider, config, sealed); err != nil {
		return "", err
	}

	return provider, settings(m, config)
}

var providers = map[string]bool{creds.ProviderStatic: true, creds.ProviderKubernetes: true, creds.ProviderVault: true}

var credentialKeys = map[string]bool{
	creds.DSNKey: true, creds.PathKey: true, creds.TemplateKey: true, creds.StoreConfigKey: true,
}

// misplaced refuses a credential field the target's provider does not read, rather than storing a setting nothing uses.
func misplaced(m *godwitv1.RegisterTargetRequest, provider string) error {
	for _, f := range []struct {
		name, owner string
		given       bool
	}{
		{"dsn", creds.ProviderStatic, m.Dsn != ""},
		{"secret_path", creds.ProviderKubernetes, m.SecretPath != ""},
		{"vault_path", creds.ProviderVault, m.VaultPath != ""},
		{"vault_template", creds.ProviderVault, m.VaultTemplate != nil},
		{"credential_store", creds.ProviderVault, m.CredentialStore != ""},
	} {
		if f.given && provider != f.owner {
			return invalid(fmt.Sprintf("%s is a setting of the %s provider, and this target's provider is %s",
				f.name, f.owner, provider))
		}
	}

	return nil
}

func (s *Server) credentials(ctx context.Context, m *godwitv1.RegisterTargetRequest, provider string, config map[string]string, sealed string) error {
	switch provider {
	case creds.ProviderStatic:
		setString(config, creds.DSNKey, orNil(sealed))
		if config[creds.DSNKey] == "" {
			return invalid("static provider requires dsn")
		}
	case creds.ProviderKubernetes:
		setString(config, creds.PathKey, orNil(m.SecretPath))
		if config[creds.PathKey] == "" {
			return invalid("kubernetes provider requires secret_path")
		}
	case creds.ProviderVault:
		setString(config, creds.PathKey, orNil(m.VaultPath))
		if config[creds.PathKey] == "" {
			return invalid("vault provider requires vault_path")
		}
		if err := setTemplate(config, m.VaultTemplate); err != nil {
			return err
		}
		setString(config, creds.StoreConfigKey, orNil(m.CredentialStore))
		if config[creds.StoreConfigKey] == "" {
			return invalid("vault provider requires credential_store, the registered Vault this target's " +
				"secret lives in; `godwit credential-stores` lists them and `godwit credential-store add` registers one")
		}
		if _, err := s.store.VaultStore(ctx, config[creds.StoreConfigKey]); err != nil {
			return invalid(fmt.Sprintf("credential store %q: %v", config[creds.StoreConfigKey], err))
		}
	}

	return nil
}

func settings(m *godwitv1.RegisterTargetRequest, config map[string]string) error {
	t := controlplane.Timeouts{
		Lock:      value(m.LockTimeout, config[controlplane.ConfigLockTimeout]),
		Statement: value(m.StatementTimeout, config[controlplane.ConfigStatementTimeout]),
	}
	if _, err := t.Options(); err != nil {
		return invalid(err.Error())
	}
	setString(config, controlplane.ConfigLockTimeout, m.LockTimeout)
	setString(config, controlplane.ConfigStatementTimeout, m.StatementTimeout)
	setBool(config, controlplane.ConfigRequirePlan, m.RequirePlan)
	setBool(config, controlplane.ConfigKeepOld, m.KeepOld)
	setBool(config, controlplane.ConfigIgnoreAdopted, m.IgnoreAdoptedTables)
	if m.SearchPath != nil {
		path, err := controlplane.ParseSearchPath(*m.SearchPath)
		if err != nil {
			return invalid(err.Error())
		}
		setString(config, controlplane.ConfigSearchPath, &path)
	}
	if m.GithubRepositories != nil {
		if err := controlplane.SetGitHubRepositories(config, m.GithubRepositories.Values); err != nil {
			return invalid(err.Error())
		}
	}

	return nil
}

var errTemplateSecret = errors.New("vault_template takes the password from the secret godwit reads, as {{password}}: " +
	"a literal one would live in the target's registration, which is configuration and is shown back")

func setTemplate(config map[string]string, template *string) error {
	if template == nil {
		return nil
	}
	for _, pw := range passwords(*template) {
		if !strings.Contains(pw, "{{") {
			return connect.NewError(connect.CodeInvalidArgument, errTemplateSecret)
		}
	}
	setString(config, creds.TemplateKey, template)

	return nil
}

func passwords(template string) []string {
	var out []string
	if _, rest, ok := strings.Cut(template, "://"); ok {
		authority, _, _ := strings.Cut(rest, "/")
		if userinfo, _, ok := strings.Cut(authority, "@"); ok {
			if _, pw, ok := strings.Cut(userinfo, ":"); ok {
				out = append(out, pw)
			}
		}
	}
	for _, field := range strings.FieldsFunc(template, func(r rune) bool { return r == ' ' || r == '?' || r == '&' }) {
		if pw, ok := strings.CutPrefix(field, "password="); ok {
			out = append(out, pw)
		}
	}

	return out
}

func setString(config map[string]string, key string, v *string) {
	switch {
	case v == nil:
	case *v == "":
		delete(config, key)
	default:
		config[key] = *v
	}
}

func setBool(config map[string]string, key string, v *bool) {
	if v != nil {
		config[key] = strconv.FormatBool(*v)
	}
}

func orNil(v string) *string {
	if v == "" {
		return nil
	}

	return &v
}

func value(v *string, current string) string {
	if v == nil {
		return current
	}

	return *v
}

func auditDetail(r controlplane.TargetRegistration) string {
	fields := r.Fields()
	out := make([]string, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		if v := fmt.Sprint(fields[i+1]); v != "" {
			out = append(out, fmt.Sprint(fields[i])+"="+v)
		}
	}

	return strings.Join(out, " ")
}
