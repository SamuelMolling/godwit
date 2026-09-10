package controlplane

import (
	"strings"

	"github.com/SamuelMolling/godwit/internal/creds"
)

// TargetRegistration is a target as it was registered: where its credential is read from and the settings
// every run on it inherits. It holds no credential — a static target's sealed DSN stays in the config map
// the providers read — so what is built from one can be shown.
type TargetRegistration struct {
	Name                string
	Provider            string
	CredentialStore     string
	VaultPath           string
	VaultTemplate       string
	SecretPath          string
	DSNRegistered       bool
	Timeouts            Timeouts
	SearchPath          string
	RequirePlan         bool
	KeepOld             bool
	IgnoreAdoptedTables bool
	GitHubRepositories  []string
}

// RegistrationOf reads a stored target config back into the settings it was registered with.
func RegistrationOf(name, provider string, config map[string]string) TargetRegistration {
	r := TargetRegistration{
		Name: name, Provider: provider, CredentialStore: config[creds.StoreConfigKey],
		Timeouts: TargetTimeouts(config), SearchPath: config[ConfigSearchPath],
		RequirePlan: config[ConfigRequirePlan] == "true",
		KeepOld:     config[ConfigKeepOld] != "false", IgnoreAdoptedTables: config[ConfigIgnoreAdopted] != "false",
		GitHubRepositories: splitRepositories(config[configGitHubRepositories]),
	}
	switch provider {
	case creds.ProviderStatic:
		r.DSNRegistered = config[creds.DSNKey] != ""
	case creds.ProviderKubernetes:
		r.SecretPath = config[creds.PathKey]
	case creds.ProviderVault:
		r.VaultPath, r.VaultTemplate = config[creds.PathKey], config[creds.TemplateKey]
	}

	return r
}

// Fields is the registration as ordered log attributes, and the only rendering of one: the audit entry
// and the server log are both built from it.
func (r TargetRegistration) Fields() []any {
	return []any{
		"provider", r.Provider,
		"credential_store", r.CredentialStore,
		"vault_path", r.VaultPath,
		"vault_template", r.VaultTemplate,
		"secret_path", r.SecretPath,
		"dsn_registered", r.DSNRegistered,
		"lock_timeout", r.Timeouts.Lock,
		"statement_timeout", r.Timeouts.Statement,
		"require_plan", r.RequirePlan,
		"keep_old", r.KeepOld,
		"ignore_adopted_tables", r.IgnoreAdoptedTables,
		"search_path", r.SearchPath,
		"github_repositories", strings.Join(r.GitHubRepositories, " "),
	}
}
