package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultScratchTemplate is what scratch databases are cloned from: template0 carries no extension an
// operator installed into template1, and a non-superuser cannot add dblink or postgres_fdw itself.
const DefaultScratchTemplate = "template0"

// ErrScratchPrivileged marks a scratch connection whose role can act outside its own scratch databases.
var ErrScratchPrivileged = errors.New("scratch role can act outside its own scratch databases")

// Scratch creates and drops the throwaway databases validation and diff run submitted SQL on.
type Scratch struct {
	pool     *pgxpool.Pool
	template string
}

// NewScratch wires scratch databases onto pool; an empty template means DefaultScratchTemplate.
func NewScratch(pool *pgxpool.Pool, template string) *Scratch {
	if template == "" {
		template = DefaultScratchTemplate
	}

	return &Scratch{pool: pool, template: template}
}

func (s *Scratch) create(ctx context.Context, name string) error {
	if _, err := s.pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+
		" TEMPLATE "+pgx.Identifier{s.template}.Sanitize()); err != nil {
		return fmt.Errorf("create scratch database: %w", err)
	}

	return nil
}

func (s *Scratch) drop(ctx context.Context, name string) {
	_, _ = s.pool.Exec(context.WithoutCancel(ctx),
		"DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

// connConfig points a copy of the scratch credentials at one scratch database.
func (s *Scratch) connConfig(name, searchPath string) *pgx.ConnConfig {
	cfg := s.pool.Config().ConnConfig.Copy()
	cfg.Database = name
	if searchPath != "" {
		cfg.RuntimeParams["search_path"] = searchPath
	}

	return cfg
}

// ScratchFinding is one way the scratch role reaches past the databases it makes for itself.
type ScratchFinding struct {
	// Fatal marks a finding that makes an isolated scratch connection pointless.
	Fatal  bool
	Detail string
}

const scratchProbeSQL = `SELECT current_user,
       current_setting('is_superuser') = 'on',
       r.rolcreaterole,
       r.rolreplication,
       r.rolbypassrls,
       pg_has_role(r.oid, 'pg_execute_server_program', 'USAGE'),
       pg_has_role(r.oid, 'pg_read_server_files', 'USAGE'),
       pg_has_role(r.oid, 'pg_write_server_files', 'USAGE'),
       EXISTS (SELECT 1 FROM pg_database d WHERE d.datname = $1 AND d.datdba = r.oid),
       (SELECT has_database_privilege(r.oid, d.oid, 'CONNECT') FROM pg_database d WHERE d.datname = $1)
  FROM pg_roles r WHERE r.rolname = current_user`

// Check reports what the scratch role may do besides making and dropping its own databases; storeDB names
// the control-plane database, which is only found when the scratch connection shares the store's server.
func (s *Scratch) Check(ctx context.Context, storeDB string) ([]ScratchFinding, error) {
	var role string
	var super, createRole, replication, bypassRLS, execProgram, readFiles, writeFiles, ownsStore bool
	var connectStore *bool
	if err := s.pool.QueryRow(ctx, scratchProbeSQL, storeDB).Scan(&role, &super, &createRole, &replication,
		&bypassRLS, &execProgram, &readFiles, &writeFiles, &ownsStore, &connectStore); err != nil {
		return nil, fmt.Errorf("inspect scratch role: %w", err)
	}
	probes := []struct {
		hit    bool
		fatal  bool
		detail string
	}{
		{super, true, "is a superuser, so submitted DDL reaches COPY ... FROM PROGRAM, pg_read_file, dblink and ALTER ROLE"},
		{ownsStore, true, fmt.Sprintf("owns the store database %q, so submitted DDL can DROP DATABASE ... WITH (FORCE)", storeDB)},
		{execProgram, true, "is a member of pg_execute_server_program, so submitted DDL runs commands on the scratch host"},
		{readFiles, true, "is a member of pg_read_server_files, so submitted DDL reads files on the scratch host"},
		{writeFiles, true, "is a member of pg_write_server_files, so submitted DDL writes files on the scratch host"},
		{createRole, true, "has CREATEROLE, so submitted DDL can grant itself the memberships above"},
		{replication, true, "has REPLICATION, so submitted DDL can stream the whole cluster"},
		{bypassRLS, false, "has BYPASSRLS"},
		{connectStore != nil && *connectStore, false, fmt.Sprintf(
			"may CONNECT to the store database %q: REVOKE CONNECT ON DATABASE %s FROM PUBLIC and from this role",
			storeDB, pgx.Identifier{storeDB}.Sanitize())},
	}
	var out []ScratchFinding
	for _, p := range probes {
		if p.hit {
			out = append(out, ScratchFinding{Fatal: p.fatal, Detail: "scratch role " + role + " " + p.detail})
		}
	}

	return out, nil
}
