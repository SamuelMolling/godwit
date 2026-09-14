# Show godwit in Backstage

`backstage-plugin-godwit` is a read-only view of godwit targets for a Backstage frontend. For each target it
shows how many migrations are applied and the version the target is at, what the newest ready plan would
still apply, where the credential is read from, the drift state with the diff of every open event and the
history of resolved ones, and the recent runs with their state, kind, who created them and when.

It has no button that changes anything: no apply, no revert, no drift accept. A mutation needs an identity
godwit can attribute, and a Backstage proxy has one token for every viewer. Changes stay with the
[GitHub App](github-app.md), the [Action](github-actions.md) and the [CLI](cli.md).

The package lives in [`backstage/`](../../backstage) and supports the legacy frontend system
(`createApp` from `@backstage/app-defaults`), from Backstage 1.38.

## What must exist first

- A godwit service the Backstage **backend** can reach. The browser never talks to godwit.
- A `read` token for the proxy. Add one entry to `GODWIT_TOKENS` on the service
  ([token spec](../run/configuration.md#token-spec)):

```bash
GODWIT_TOKENS='...,backstage:read:<secret>'
```

`backstage` is the name the access log records for every call the plugin makes. `read` is enough for every
RPC the plugin calls: `GetTargetStatus`, `ListTargets`, `ListPlans`, `ListDriftEvents` and `ListRuns`.

## The proxy entry

The token is injected by Backstage's proxy on the server side, so it never reaches a browser:

```yaml
proxy:
  endpoints:
    '/godwit':
      target: 'https://godwit.internal.example.com'
      credentials: require
      allowedMethods: ['POST']
      headers:
        Authorization: 'Bearer ${GODWIT_BACKSTAGE_TOKEN}'
```

Put it beside the proxy entries the app already has, in the same form. An app that still declares its
entries directly under `proxy:` (`proxy: '/github/api': ...`) must add `'/godwit'` there too: once
`proxy.endpoints` exists, Backstage ignores every entry declared directly under `proxy:`.

The proxy lets through anything the token allows, not only what the plugin calls. With a `read` token that
is every `read` RPC — including `GetRun` and `GetPlan` with the migration files, `ListAudit`, and `PlanRun`
and `Diff`, which replay SQL on the scratch database — for every signed-in Backstage user.
`credentials: require` (the default) keeps it to signed-in users.

A proxy path other than `/godwit` is set in `app-config.yaml`:

```yaml
godwit:
  proxyPath: /godwit-production
```

## Install

```bash
yarn --cwd packages/app add backstage-plugin-godwit
```

Register the plugin in `packages/app/src/App.tsx`, so its API is available wherever the view is rendered:

```tsx
import { godwitPlugin } from 'backstage-plugin-godwit';

const app = createApp({
  apis,
  plugins: [godwitPlugin],
});
```

## On a catalog entity page

Name the targets in the entity's `catalog-info.yaml`:

```yaml
metadata:
  annotations:
    godwit/targets: orders-staging, orders-production
```

The value is a comma-separated list of target names as they are registered on the service. Spaces around a
name are ignored, a repeated name is shown once, and the targets are shown in the order written. One target
is written the same way, without a comma. A target whose name contains a comma cannot be annotated; pass it
to the view directly.

Add a tab to the entity page in `packages/app/src/components/catalog/EntityPage.tsx`:

```tsx
import { EntityGodwitContent, isGodwitAvailable } from 'backstage-plugin-godwit';

<EntityLayout.Route path="/godwit" title="Migrations" if={isGodwitAvailable}>
  <EntityGodwitContent />
</EntityLayout.Route>
```

or inside an `EntitySwitch`:

```tsx
<EntitySwitch>
  <EntitySwitch.Case if={isGodwitAvailable}>
    <EntityGodwitContent />
  </EntitySwitch.Case>
</EntitySwitch>
```

An entity without the annotation renders Backstage's missing-annotation notice.

## Embedded in a page that is not an entity page

`GodwitTargetsView` takes the target names as props and needs no `EntityProvider`, so any page can render it
— one that selects an application and an environment, for example:

```tsx
import { GodwitTargetsView } from 'backstage-plugin-godwit';

<GodwitTargetsView targets={[`${app}-${environment}`]} />
```

Both components take `maxRuns` (default `10`), the number of recent runs shown per target.
`getGodwitTargets(entity)` and `parseGodwitTargets(value)` read the annotation for a page that has an entity
in hand but renders the view itself.

## What each part of the view means

| Field | Where it comes from |
|---|---|
| Applied, At version | `GetTargetStatus`: the versioned migrations in the target's own journal, and the newest of them. `unknown`, with the reason, when the target's credential does not resolve. |
| Pending | `ListPlans`: the migrations of the newest `ready` plan that are not applied. godwit only knows what is pending against a set of files, and a stored plan is the set it has; `no ready plan` means none is bindable. |
| Provider, Credential store | `ListTargets`. A `vault` target with no registered store reads `VAULT_ADDR`, the service's own. |
| Runs needing attention | `ListTargets`: runs in `needs_attention` or `awaiting_contract`. |
| Drift | `ListDriftEvents` and the baseline from `GetTargetStatus`. Every open event is shown with its diff: `+` is in the live schema and not in the baseline, `-` is in the baseline and gone from the live schema. Resolved events are listed with the time they were detected and resolved. |
| Recent runs | `ListRuns`, newest first. |

Drift has four states, and the view keeps them apart:

- **drifted** — at least one open event. Resolve it with a migration, by undoing the change, or with
  [`godwit drift accept`](cli.md#godwit-drift-accept) when the change is meant to stay.
- **no drift** — no open event, and a baseline exists to compare against.
- **not watched** — no baseline yet; one is taken after the target's first successful run.
- **Could not read drift** — the call failed. The view never shows this as clean.

*Created by* is the run's `created_by` ([actors and provenance](../internals/runs.md#actors-and-provenance)):
`github:acme/orders:alice` reads `@alice`, *GitHub App on acme/orders*; `github:acme/orders:(autoplan)` reads
`autoplan`; `ui:oncall` reads `oncall`, *godwit UI*; a token name reads as itself.

## Developing the package

```bash
make backstage   # npm ci when the lockfile changed, then tsc, lint and the tests at 100% coverage
```

`scripts/coverage.sh`, which `make cover` and CI run, calls the same script when `npm` is on `PATH` and says
`backstage/ NOT TESTED` when it is not. The lockfile resolves the `@backstage/*` packages Backstage 1.38
shipped, so the tests run against the oldest release the package supports.

Publishing is `npm publish` from `backstage/` after `npm run build`; `prepack` rewrites `main` and `types` to
the build output.
