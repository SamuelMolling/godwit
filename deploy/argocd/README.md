# ArgoCD hooks

Two Jobs wrap an application's sync with an expand → contract migration. Copy them next to the app's manifests and rename `orders`. Both run `ghcr.io/samuelmolling/godwit:main`; pin a `sha-<short commit>` tag in a production overlay.

```
PreSync   godwit migrate --target orders --dir /migrations --rollout expand-contract
Sync      the app rolls out on the expanded schema
PostSync  godwit run confirm --latest --allow-none --target orders
```

- [presync-job.yaml](presync-job.yaml) — sends the migrations to the service and streams the run. Exit 0 on `succeeded` or `awaiting_contract` (contract statements held back), 1 on `failed` / `needs_attention`, which fails the sync before any pod changes.
- [postsync-confirm.yaml](postsync-confirm.yaml) — once the new pods are healthy, confirms the newest run on the target still in `awaiting_contract`. `--allow-none` makes it a no-op when the PreSync run held nothing back (or there were no new migrations), so every sync stays green.

Both use `hook-delete-policy: BeforeHookCreation`: the previous Job stays around for `kubectl logs` until the next sync replaces it. `backoffLimit: 0` because the service already retries runs itself; a second Job would just create a second run.

## Where the migrations come from

The PreSync Job mounts a ConfigMap named `orders-migrations` at `/migrations`. A ConfigMap keeps the hook self-contained and versioned with the manifest that ArgoCD is syncing: the migrations the hook applies are exactly the ones in the revision being deployed, and the godwit image stays distroless (no shell, no copy step). Migrations are tiny compared to the 1 MiB ConfigMap limit.

Helm chart of the app:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: orders-migrations
data:
  {{- (.Files.Glob "db/migrations/*.sql").AsConfig | nindent 2 }}
```

Kustomize (files must be listed; the generator does not glob):

```yaml
configMapGenerator:
  - name: orders-migrations
    options:
      disableNameSuffixHash: true
    files:
      - db/migrations/20260901120000_create_orders.up.sql
      - db/migrations/20260901120000_create_orders.down.sql
```

Alternative rejected: an init container copying `db/migrations` out of the application image into an `emptyDir`. It works when the app image has a shell, but it ties the hook to the app image's layout and adds a container per sync for no gain.

## Token

Both Jobs read `GODWIT_TOKEN` from the `orders-godwit` Secret. Use a dedicated token per application (one entry of `GODWIT_TOKENS` on the service) so it can be rotated without touching the others.
