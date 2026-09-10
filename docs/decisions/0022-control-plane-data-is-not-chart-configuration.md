# 0022 — Control-plane data is not chart configuration

Shipped in #138. It deletes `targets.enabled`, `targets.list`, `stores.list` and the register Job from the Helm chart. `stores.allowedHosts` stays, and the reason it stays is the whole line this record draws. ([0023](0023-the-token-godwit-presents-is-minted-for-the-vault-it-goes-to.md) later deleted `stores.allowedHosts` too, having closed the escalation it guarded rather than the line this record draws; the `stores.audiences` it put there in exchange went the same way once the audience became a constant.)

## The question

The chart could declare targets, and did. `targets.list` was a list of `godwit target add` lines rendered into a Job; `stores.list` was the same for `godwit credential-store add`, added by hand hours after [0021](0021-a-target-names-the-vault-its-credentials-live-in.md) created credential stores, because the chart already offered the shape.

The objection, on reading it: *if it is a database, why is it in git?*

## The decision

**A row the control plane owns is not declared in the values file of the process that owns it.**

A target is a row in `cp_targets`. A credential store is a row in `cp_credential_stores`. Both are created by an API call carrying an `admin` token, written to the store, recorded in `cp_audit` with the actor that made them, and referenced by every run, plan and drift event afterwards. A values file that also holds them gives one row two sources of truth.

It is not a tie. `RegisterTarget` is an upsert that writes a **new config map, not a patch** — the property 0021 and [deployment](../deployment.md#re-registering-replaces-the-whole-row) both state plainly. So the declaration wins, always and quietly: a target registered through the API, or a flag added to one, is gone at the next sync with no error, no diff and no event. That is the failure shape where two systems each believe they own one fact, and the one that syncs on a timer wins.

The values file is also the worse of the two owners. It carries no actor, so `cp_audit` records the Job; it cannot be applied without a release; and it puts the set of databases godwit migrates into the deployment's own configuration, where it is neither reviewed by the people who own those databases nor visible to `godwit targets`.

**Nobody was using it.** `targets.enabled` defaulted to `false`, the live deployment left it there, and both of its targets were registered through the API. That is not what saves the removal from being a breaking change — it is why the keys were worse than absent: the next person reads `values.yaml`, finds the block, and believes it is the supported path.

### Where the line falls

The test is not whether a setting is important, or secret, or hard to type. It is **who owns the fact, and what happens when two things assert it.**

| In the chart | Not in the chart |
|---|---|
| `serve.keyProvider.*` — which KMS seals `static` DSNs, a property of this process | a target, a credential store |
| `existingSecret.*` — which Secret this Deployment reads, and under which keys | the DSN behind a target |
| `serve.port`, `serve.githubApp.*`, the routes — what this process listens on and where it is published | which repositories are bound to a target |
| `serve.limits.*`, `serve.driftInterval` — how this process behaves | what it behaves *on* |
| `stores.allowedHosts` — the hosts an admin may register a store at | the stores themselves |

`stores.allowedHosts` is the interesting one, and it survives on its own argument, made in [0021](0021-a-target-names-the-vault-its-credentials-live-in.md#constraining-what-a-store-may-point-at): it exists precisely to move *which Vaults godwit may authenticate at* out of the admin token — which lives in a Secret, held by CI — and into the pod spec, which lives in git and is reviewed. It constrains registration; it does not perform it. Two of them cannot disagree, because there is only one process. Deleting it alongside `stores.list` would have removed the only control over the escalation `stores.list` was a way of exercising.

`--vault-host` is a flag of `godwit serve`, so `serve.vaultHosts` would be the tidier home for it. It stays at `stores.allowedHosts`: the rename would be a second migration for every deployment already setting it, bought with nothing but symmetry.

## What it costs

**A deployment that used the keys must register its targets some other way**, which is one Job running the same `target add` lines the chart's Job ran, in the repository that owns the target. The chart takes it in `extraObjects` if it wants to stay in the release. Nothing about the shape is lost — it moves to where the target is owned, which is where the full `target add` line belongs anyway, since a shorter one drops settings.

**The chart no longer has an answer to "how do I get a target".** `NOTES.txt` and the README now print the commands instead, which is the honest version: there is one supported path, and it is the API.

**This supersedes one sentence of 0021** — "the chart's Job registers `stores.list` ahead of `targets.list` for that reason", under *What it costs*. That record is left as it was written; the ordering it describes is now the operator's, and a store still has to exist before a target names it.

## Rejected

| Thing | Verdict | Reason |
|---|---|---|
| Keeping `targets.list` and documenting the hazard | refused | It was documented — `values.yaml`, the chart README and `deployment.md` all said the list is authoritative and a hand-registered setting is lost. It stayed the default-looking path anyway. A comment does not fix a design that has two owners for one row. |
| Keeping it behind `targets.enabled: false` | refused | That is what it already was. Off by default is not off; it is a loaded key waiting for someone who reads `values.yaml` as the menu of what is supported. |
| A `--dry-run` or reconciling mode that warns on drift instead of overwriting | refused | It would build a reconciler for rows the API already owns, and its warning has nowhere to go — a Job's log is not read on a green sync. The reconciler that exists is the operator's Job running `target add`. |
| Deleting `stores.allowedHosts` with `stores.list` | refused | It is not a store. It is the only constraint on what an admin token may make godwit authenticate at, and 0021 put it in the pod spec on purpose. |
| Moving `stores.allowedHosts` to `serve.vaultHosts` | refused | Correct by the values file's own organising principle, and not worth a second breaking rename in the same change. |
| Removing `godwit target add` / `credential-store add` from the CLI too | refused | They are not the defect. A person or a Job running one command with an admin token is the supported path, and the removal is what makes it the *only* path. |
