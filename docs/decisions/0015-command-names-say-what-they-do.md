# 0015 — Command names that say what they do

> **Status: decided.** The four questions below were argued in `open/cli-command-names.md` and
> answered by the owner: **all four recommendations taken.** `plan` keeps its name and gains
> `--save`; `baseline` and `reconcile` become one `target adopt`; `drift accept` is documented
> rather than renamed; the local `apply` becomes `up`. Nothing was released, so there are no
> aliases and no deprecation shims — a clean break was the point.
>
> Everything below the horizontal rule is the analysis as it was written *before* the decision,
> preserved unedited, because the evidence is what the decision rests on. The commands it quotes
> are the ones that existed then. **[What shipped](#what-shipped)** is at the end of this record.

## The question

*The paragraphs from here to the recommendation are the open record as it stood.*

Writing [`docs/cli.md`](../cli.md) (#106) meant defining every command in one sentence. Three
resisted, and in each case the resistance came from the command rather than from the prose. A
fourth pair — `revert` and `down` — was suspected and is largely **cleared**; what the check turned
up instead is a fifth problem nobody had listed, and it is the worst one in the repository.

This record asks whether any of them should be renamed. **Nothing is released** — version `0.0.1`,
`git tag` returns nothing, the Homebrew tap in `.goreleaser.yaml` has never published a bottle — so
this is the cheapest moment a rename will ever have. It is also the moment at which the argument
*for* renaming is weakest, because nobody has been confused by it yet except the person writing the
documentation. Both facts are stated plainly rather than used to settle it.

**Four things this review found, before the argument starts.**

1. **`plan` is not two commands, it is three**, and the flag that picks between them can come from
   a file. `godwit plan --dir X` and `godwit plan --target app` share nothing but the word: one
   calls `engine.LoadDir`, the other calls `PlanRun` over the network. And `PlanRunRequest` already
   carries a `persist` field the CLI never exposes.
2. **`apply` already means two opposite things in this repository, and the docs say so 80 times.**
   66 of those 80 occurrences mean `godwit migrate`. This is the one problem documentation cannot
   fix, because the documentation is what is inconsistent.
3. **`baseline` means two unrelated things**, and neither the CLI nor the store notices. It is the
   adoption command *and* the drift reference — `godwit target baseline` writes migration history;
   `godwit drift accept` prints `baseline accepted` and calls an RPC named `AcceptBaseline`.
4. **The UI already solved `drift accept`.** Its button reads **Accept as baseline** and carries a
   one-line note naming the consequence. The CLI prints `target app: baseline accepted` and names
   none. That is a prose gap on one surface, not a misnamed command.

---

## What the CLI actually does

Four distinct operations sit under two names. The rows are ordered by blast radius.

| You type | Code path | Connects to | Writes |
|---|---|---|---|
| `godwit plan --dir X` | `engine.LoadDir` + `engine.BuildPlan`, in process (`commands.go:233`) | nothing | nothing |
| `godwit migrate --dry-run` | `PlanRun{persist:false}` (`service.go:219`) | service, target, scratch database | nothing durable |
| `godwit plan --target app` | `PlanRun{persist:true}` (`service.go:242`) | service, target, scratch database | `cp_plans`, `cp_plan_files`, audit `plan.create` |
| `godwit migrate` | `CreateRun` | service, target, scratch database | **the database** |

Three facts follow from that table, and they are the evidence for the whole of problem 1.

**The `persist` flag is real and already in the wire format.** `PlanRunRequest.persist` is field 7,
documented in the proto as *"Store the plan so a later CreateRun with the same migration set binds
to it."* The CLI hardcodes it to `true` in `persistPlan` and to `false` in `dryRun`. The API has the
distinction the CLI has collapsed: **two CLI commands cover one RPC, while one CLI command covers
two unrelated code paths.**

**The service form is classified as a read.** `GodwitServicePlanRunProcedure` maps to `ScopeRead`
(`internal/api/auth.go:50`), and `docs/configuration.md` lists `godwit plan --target` under
`Service` with scope `read`. It nevertheless inserts rows into `cp_plans` and `cp_plan_files`,
writes an audit row, and submits your DDL to a scratch database — which `godwit serve` warns about
in the strongest language on the page: *"validation and diff execute submitted DDL on the store
server with the store credentials."* A `read`-scoped token does all of that.

**The switch can arrive from a file.** `plan` registers `target` as a `godwit.yaml` key
(`commands.go:263`), so in a repository whose `godwit.yaml` names a target, a bare `godwit plan`
is the service form. `docs/configuration.md` already carries a paragraph warning about exactly this
and an escape hatch:

> `target` reaches `plan` as well as `migrate`, and `plan --target` is a service command: once
> `godwit.yaml` names a target, a bare `godwit plan` no longer parses the directory offline — it
> plans against the live target and stores a plan on the service. `godwit plan --target ""` forces
> the offline form back.

**That paragraph is the strongest argument in this record.** Documentation was already tried here.
It required a dedicated warning *and* the invention of `--target ""` as an un-setter. A command
whose documentation has to ship an escape hatch is not a documentation problem.

---

## Problem 1 — `godwit plan` is two commands under one name

**What is confusing.** The word `plan` covers an offline parser that connects to nothing and writes
nothing, and a service call that reads the live target, replays every pending migration on a scratch
database, and stores a durable artifact a later `migrate` binds to and refuses to diverge from. The
docs cannot describe it in one sentence because there is no sentence: `cli.md` splits the section in
two ("Without `--target` it is entirely offline"; "With `--target` it becomes a different, more
useful thing"), and `configuration.md` lists `godwit plan` in the **Local (no service)** table *and*
`godwit plan --target <name>` in the **Service** table. The reference is already two entries. The
CLI is one.

### The options

| | Option | After the change | What muscle memory unlearns |
|---|---|---|---|
| **1a** | **Do nothing** | unchanged | nothing |
| **1b** | **Make the durable half explicit, and stop the file from choosing** — the stored form needs `--save`, and `target` is dropped from `plan`'s `godwit.yaml` keys | `godwit plan --dir X` (offline) · `godwit plan --target app` (live, prints, stores nothing) · `godwit plan --target app --save` (stores) | CI must add `--save`, and must spell `--target` instead of inheriting it. `--target ""` stops being needed and should be removed from the docs. `migrate --dry-run` becomes redundant with the middle row and can be documented as its alias |
| **1c** | **Split the name** — `plan` becomes offline-only and refuses `--target`; the service form gets its own verb | `godwit plan --dir X` (offline, unchanged) · `godwit propose --target app` (live, stores; `--dry-run` to print without storing) | every service-side `plan` invocation and the Action's `command: plan` value. `plan --target` starts erroring instead of working differently |
| **1d** | **Split the other way** — `plan` becomes the service command only, and the offline form folds into `lint`, which already parses every file with the same parser | `godwit lint --dir X --show-statements` · `godwit plan --target app` | the offline `plan` disappears; `docs/getting-started.md`'s first plan example is rewritten |

### The cost, counted

`godwit plan` appears on **72 lines across 33 files**: 51 in `docs/` and `README.md`, 14 in Go
(9 of them tests), 3 in `examples/`, 2 in `.github/workflows/action-smoke.yml`, 1 in
`scripts/action-run.sh`, 1 in `deploy/argocd/README.md`. 24 of the 72 carry `--target` on the same
line. Beyond the literal string:

- **`godwit.yaml` keys: zero change.** No key is named after a command. Command names appear only in
  the *Used by* column of the key table, where `plan` occupies **5 of the 8 rows**. Option 1b
  removes `plan` from the `target` row — one cell — and deletes the warning paragraph quoted above.
- **GitHub Action: 1 binary invocation** (`scripts/action-run.sh`), plus the `command: plan` input
  value if the Action's vocabulary is kept aligned — **16 lines in `action.yml`, 39 in
  `docs/ci-cd.md`** — and the `<!-- godwit:plan -->` sticky-comment marker. Option 1b changes none
  of this except adding `--save` to one invocation; options 1c and 1d change all of it.
- **Helm chart: 1 line** (`deploy/argocd/README.md`; the chart itself never mentions `plan`).
- **RPC names: zero.** `PlanRun` need not move under any option. Option 1b exposes a field that
  already exists.
- **Audit strings: zero.** `plan.create` and `plan.supersede` are `text` in `cp_audit`
  (`schema.go:152`), so any rename of them splits stored history across two spellings. Not renaming
  them is the right answer, which means the CLI name and the audit name are decoupled anyway.

### What the corpus calls this

**Terraform is the idiom every user has already met**, and it is option 1b exactly:
`terraform plan` *"creates an execution plan, which lets you preview the changes"*; `-out=FILE`
*"save the generated plan to a file on disk, which you can later execute by passing the file to
`terraform apply`"*; and `terraform apply` without a file *"automatically creates a new execution
plan as if you had run `terraform plan`"*
([plan](https://developer.hashicorp.com/terraform/cli/commands/plan),
[apply](https://developer.hashicorp.com/terraform/cli/commands/apply)). One verb, one flag,
throwaway by default and durable on request. godwit's `migrate` already re-plans when no stored plan
matches — `no stored plan for this set: implicit plan` — so the whole Terraform shape is present
except the flag.

**Atlas is the one tool that uses a different verb**, and it is the argument for 1c:
`atlas migrate apply --dry-run` is the throwaway preview, while `atlas schema plan` *"generates a
migration plan for the specified Schema Transition (State1 -> State2) and stores it in the Atlas
Registry"*, `atlas schema plan approve` promotes it, and `atlas schema apply --plan <id>` binds it
([declarative/plan](https://atlasgo.io/declarative/plan)). Note what Atlas separates: not
offline-vs-online, but **preview vs. approved artifact**, and the verb it reaches for is *approve*.
Nobody in the corpus uses a distinct verb for godwit's actual split, which is *offline parse* vs.
*live plan* — because no other tool has an offline parse worth a command.

Nowhere else has a durable plan at all: Flyway's `dryRunOutput` is a file you read (Teams edition),
Liquibase's `update-sql` *"allows you to inspect the SQL Liquibase will run"*
([docs](https://docs.liquibase.com/commands/update/update-sql.html)), Alembic's `upgrade --sql`
prints offline, and golang-migrate has no preview at all.

---

## Problem 2 — `baseline` and `reconcile` both mean "adopt"

**What is confusing.** Nothing in either name says which one you want, and the difference is not
what they do — both write a `succeeded` run holding files they put on the books without executing
them, and both take a drift snapshot — but **where the truth comes from**. `baseline` takes a
version you supply and trusts you; `reconcile` reads the target's own `godwit` journal and trusts
it. `cli.md` had to define each by when *not* to use it, and both definitions point at the other
one. The proof that the parent concept exists is in `concepts.md`, which already groups them under
one heading — *"Adopting an existing database — Two ways in, depending on what the database already
carries"* — and then describes two commands that do not share a word.

The failure modes are asymmetric in a way the names hide. `baseline` is the dangerous one:
`cli.md` has to warn that *"getting the version wrong in either direction hurts: too low and godwit
will try to re-run migrations the database already has, too high and it will silently skip ones it
does not."* `reconcile` cannot be got wrong — *"there is no version for you to get wrong"* — and
refuses on three named disagreements rather than guessing. The safe one is the one with the
unfamiliar name.

`baseline` also collides with itself. `godwit drift accept` prints `baseline accepted`,
`godwit target status` prints `drift baseline: taken … by run …`, the RPC is `AcceptBaseline`, and
the UI's button is **Accept as baseline** — all about the drift fingerprint, none about migration
history. One word, two unrelated subsystems.

### The options

| | Option | After the change | What muscle memory unlearns |
|---|---|---|---|
| **2a** | **Do nothing** | unchanged | nothing |
| **2b** | **One command, and the flag names the input** — the shape `concepts.md` already describes | `godwit target adopt <name> --version 20260901120000` · `godwit target adopt <name> --from-journal` (mutually exclusive, one required) | two command names become one. `baseline` is freed to mean only the drift reference |
| **2c** | **Keep `baseline`, rename `reconcile`** — `baseline` is the corpus word and should stay | `godwit target baseline <name> --version N` (unchanged) · `godwit target import <name>` | one name. Leaves the "which one?" question answered only by prose, and leaves `baseline` overloaded |
| **2d** | **Name the source in both** | `godwit target adopt-at-version <name> --version N` · `godwit target adopt-from-journal <name>` | two names, both longer. Unambiguous and ugly; no tab-completion prefix shared with anything |

### The cost, counted

`godwit target baseline` appears on **14 lines across 8 files** (13 in `docs/`, 1 in
`deploy/helm/godwit/README.md`). `godwit target reconcile` appears on **19 lines across 11 files**
(15 in `docs/`, 3 in Go — one of them `internal/api/plan.go`, where the refusal message tells the
operator what to run — and 1 in the Helm README). **33 lines, 17 files, no overlap with the
GitHub Action at all** (`scripts/action-run.sh` invokes only `diff`, `lint`, `migrate`, `plan`,
`revert`, `run`, `runs`), **no `godwit.yaml` key**, **1 cell** in the key table's *Used by* column,
**2 lines** in the Helm chart's README and none in its templates or values.

**RPCs need not move**, and should not: `BaselineTarget` and `ReconcileTarget` describe two distinct
store operations with different refusal sets, and they are named in `concepts.md` and
`decisions/0014` as mechanisms. Same for the audit strings `target.baseline` and `target.reconcile`,
which are `text` in `cp_audit` and would split history. Under 2b the CLI has one verb over two RPCs
— which is what a CLI is for.

The one real cost 2b carries: the refusal in `internal/api/plan.go` prints
`run `godwit target reconcile app --dir <migrations>` to adopt what it already has`. That string is
a server-side instruction naming a client command, so the service and the CLI must ship together.
They already do.

### What the corpus calls this

**`baseline` is settled vocabulary and godwit is using it correctly.** Flyway's `baseline` command
*"is intended to make it easy to turn any preexisting production database into a Flyway database"*
with a `baselineVersion` you supply; Atlas has both a `--baseline` flag on `migrate apply` — *"Atlas
will mark this version as already applied and proceed with the next version after it"*
([versioned/apply](https://atlasgo.io/versioned/apply)) — and `atlas migrate set <version>`, which
*edits the revision table to consider all migrations up to and including the given version to be
applied*, without running SQL ([troubleshoot](https://atlasgo.io/versioned/troubleshoot)); pgroll's
`baseline` is how you *"'import' the current structure"* of an already-populated database
([README](https://github.com/xataio/pgroll)). Alembic calls it `stamp`, Django `migrate --fake`,
golang-migrate `force <version>`. Rails has no word for it at all.

**`reconcile` appears nowhere in this corpus.** It is a Kubernetes/GitOps word, and a reader who
knows it from there will read it as "converge continuously", which is the opposite of a one-shot
adoption. (Absence of evidence: nine tools were checked and none used it as a command name.)

**Two tools split the operation the way godwit does, and both use one verb with two forms.**
Liquibase: `changelog-sync` (adopt everything the changelog holds) and `changelog-sync-to-tag <tag>`
(adopt up to a point you name)
([docs](https://docs.liquibase.com/reference-guide/database-inspection-change-tracking-and-utility-commands/changelog-sync-to-tag)).
Django: `migrate --fake <target>` (you name it) and `--fake-initial`, which *"skip[s] an app's
initial migration if all database tables … already exist"* — it inspects the database instead of
asking you ([docs](https://docs.djangoproject.com/en/6.0/topics/migrations/)). **No tool in the
corpus uses two unrelated command names for this.** godwit is the outlier, and 2b is the shape both
precedents converge on.

---

## Problem 3 — `drift accept`

**What is confusing.** Nothing about the name, on inspection. The complaint recorded in `cli.md` was
that no plain framing avoids sounding like silencing an alarm, so the reference documented the
consequence instead: *"an accepted drift is a change no migration file describes, so the next
database you build from those files will not have it."* That paragraph works. The command is
`accept`, and what it does is accept.

**What is actually wrong is narrower and cheaper.** Four surfaces name this operation and the CLI is
the only one that does not explain it:

| Surface | What it says |
|---|---|
| RPC | `AcceptBaseline` |
| UI button (`templates/drift.html:17`) | **Accept as baseline** |
| UI note (`templates/drift.html:19`) | *"records the current schema as the new reference for {{.Target}}. It does not change the database; future checks compare against it."* |
| CLI `Short:` (`service.go:706`) | `Bless the live schema as the new baseline` |
| CLI output (`service.go:713`) | `target app: baseline accepted` |

The UI's note is the sentence the CLI is missing. "Bless" appears nowhere else in the product and
in no other tool's documentation.

### The options

| | Option | After the change | What muscle memory unlearns |
|---|---|---|---|
| **3a** | **Document, and port the UI's sentence into the CLI** — rewrite `Short:` and add the consequence to the printed line and to `runbook.md` | `godwit drift accept <target>` (unchanged); output gains *"future checks compare against it; no migration file describes this change"* | nothing |
| **3b** | **Align the name with the other four surfaces** | `godwit drift baseline <target>` | one name. Re-overloads `baseline`, which problem 2 is trying to free — these two options are in tension and should be decided together |
| **3c** | **Make the consequence a required flag** | `godwit drift accept <target> --no-migration-describes-this` | a nagging flag on an operator command that is already `ScopeOperator` and already audited. Listed to be refused |

### The cost, counted

`godwit drift accept` appears on **11 lines across 9 files** — 7 in `docs/`, 3 in Go (two tests and
`internal/api/plan.go`), 1 in `deploy/argocd/README.md`. **No GitHub Action surface, no Helm
templates, no `godwit.yaml` key.** 3a costs two Go lines and three doc lines and renames nothing.
3b costs the 11 lines plus a decision about `AcceptBaseline`, `drift.accept` and the UI button.

### What the corpus calls this

**No tool in the corpus has an `accept` verb for drift, and the words they do use are worse.**
Terraform's is `apply -refresh-only`, described as *"you can apply this plan to record the updated
values in the Terraform state without changing any remote objects"* — the verb is **record**
([tutorial](https://developer.hashicorp.com/terraform/tutorials/state/refresh)). Flyway detects
drift against stored **snapshots** — *"store snapshots in the Flyway snapshot history table on
deployment"* — and its remediation vocabulary is *revert* the drift or *incorporate* it into version
control ([drift analysis](https://documentation.red-gate.com/flyway/flyway-concepts/drift-analysis)).
Liquibase has `snapshot` as a comparison input, not an accept action. Atlas ships **Drift
Detection** and, in what was checked, no accept verb at all.

That is a real finding for this record: **`accept` is the clearest word anyone in this space has
produced for this operation**, and the tools that avoided the word did so by not shipping the
operation. A command whose consequence needs explaining is not necessarily misnamed.

---

## Problem 4 — `revert`/`down`, and the collision the check turned up

The suspicion was that `revert` and `down` both undo and neither name says which is the production
one. **Checked, and largely cleared — the pair is fine and the split behind it is real.**

The code enforces it. `targetFlags.executor` — the only place in the CLI that opens a PostgreSQL
connection — has exactly three callers: `apply` (`commands.go:714`), `status` (`:787`) and `down`
(`:845`). They are also the only three commands that register `--dsn` for themselves
(`register(cmd, true)`, three call sites). Everything else reaches the service through
`clientFlags`. The one apparent exception proves it: `godwit target add` takes both `--dsn` and
`--server`, but that DSN is a credential it hands the service to store, not a connection the CLI
opens.

`cli.md` already draws the line in prose, on `down`: *"Do not reach for it against anything shared —
it is deliberately unguarded compared to `revert`, which is the production path."* And `revert` is
not `down`'s remote twin in any case — they undo different units. `down --version N` undoes one
migration by version. `revert [run-id]` undoes **one run**, which may be several migrations, read
from the ledger, newest first, refusing data loss without `--allow-data-loss` and refusing an older
run without `--force` (`decisions/0005`). Two different operations that happen to both go backwards.
That is not a naming collision.

**But `apply` is.** The Action's `command: apply` runs `godwit migrate` — `scripts/action-run.sh:317`
dispatches `apply)` to `cmd_migrate`, and `docs/ci-cd.md` states it directly: an `issue_comment`
carrying `/godwit apply` runs *"`migrate` from the pull request head, bound to the stored plan"*. So
in the Action's vocabulary `apply` is the service-mediated production path with a review anchor and
a `pipeline` token; in the CLI, `godwit apply` is the unregistered, unaudited, no-service path that
`cli.md` tells you not to point at a managed database.

The count is not close. The string `godwit apply` occurs **80 times** in the repository:

| Spelling | Occurrences | Means |
|---|---|---|
| `/godwit apply` | 62 | `godwit migrate` (the Action's PR comment) |
| `## godwit apply` | 4 | `godwit migrate` (the Action's report heading) |
| `godwit apply` | 14 | the CLI's local, no-service command |

**66 of 80 mentions of `godwit apply` in this repository mean `godwit migrate`.** And the Action
never invokes the CLI's `apply` at all — `scripts/action-run.sh` calls only `diff`, `lint`,
`migrate`, `plan`, `revert`, `run` and `runs`. There are additionally two *hypothetical* uses of the
word in `decisions/0003` and `decisions/README.md`, where a refused declarative
`godwit apply schema.sql` is a third thing again.

### The options

| | Option | After the change | What muscle memory unlearns |
|---|---|---|---|
| **4a** | **Do nothing** | unchanged | nothing. `/godwit apply` stays the Atlantis idiom `ci-cd.md` deliberately copied, and the collision stays |
| **4b** | **Rename the local trio to the pair the corpus already teaches** | `godwit up --dsn …` · `godwit status --dsn …` · `godwit down --dsn …` (local) against `godwit migrate` · `godwit target status` · `godwit revert` (service) | `godwit apply` becomes `godwit up`. `apply` is freed to mean, everywhere in the product, the thing the Action already means by it |
| **4c** | **Rename the Action's vocabulary instead** | `command: run` (was `apply`), keeping the `/godwit apply` comment trigger, which is not a CLI command name | the Action's input value, its report heading and its docs. Every consumer's workflow file breaks |
| **4d** | **Group the local commands under a parent** | `godwit local apply` · `godwit local status` · `godwit local down` | three names gain a prefix; the split becomes explicit in the tree rather than in the vocabulary |

### The cost, counted

4b touches **14 occurrences across 8 files**: 13 in `docs/` — 5 in `cli.md`, 3 in
`getting-started.md`, and 1 each in `runbook.md`, `configuration.md`, `decisions/README.md`,
`decisions/0003` and `decisions/0014` — plus 1 Go comment in
`internal/controlplane/adoption_test.go`. The two `## godwit apply` assertions in
`.github/workflows/action-smoke.yml` match the Action's report heading and are **untouched** by a
CLI rename, which is the whole point. Plus **3 cells** in the `godwit.yaml` *Used by* column
(`dir`, `lock_timeout`, `statement_timeout`), **zero** Action inputs, **zero** Helm values,
**zero** RPCs (there is no RPC — that is the point), **zero** audit strings.

4c touches **19 lines in `action.yml`, 53 in `docs/ci-cd.md`, 30 in `action-smoke.yml`**, the
`<!-- godwit:migrate -->` marker's documentation, and every workflow anyone has copied out of
`examples/github-actions/`.

**4b is roughly a twentieth of the cost of 4c and fixes the same collision.**

### What the corpus calls this

`up`/`down` is the pair with the widest recognition in exactly the dev-loop position godwit uses it:
golang-migrate is `up`/`down`, Rails is `db:migrate`/`db:rollback`, Alembic is
`upgrade`/`downgrade`, Django overloads `migrate` in both directions. `apply` in this space belongs
to the declarative tools — Atlas (`migrate apply`, `schema apply`) and, by inheritance from
`kubectl`, the GitOps flow the Action copies.

And the corpus confirms the collision is a real hazard rather than a pedantic one: **no tool
disambiguates local-apply from server-mediated-apply lexically.** Atlas uses `apply` for both a
plain CLI run against a URL and a Registry-gated, approval-bound run; Terraform uses `apply` for a
local run and for one executed on a remote runner. Everyone lives with it — but nobody has *two
different commands* called `apply` the way this repository effectively does.

---

## Why the timing matters, and the honest limit of that argument

`git tag` returns nothing. The version is `0.0.1`. `.goreleaser.yaml` declares a Homebrew tap that
has never published. No workflow outside this repository pins a godwit command name. **Therefore
no alias is needed and no deprecation period is possible, because there is nothing to deprecate.**
Cobra would make an alias trivial — `Aliases: []string{"apply"}` is one line — and this record
recommends **against** adding any: an alias would preserve the collision it is meant to remove, and
would have to be documented, which is the cost the rename exists to avoid.

The honest limit: *"rename now, it is cheap"* is an argument about price, not about need. Every
option above has a `do nothing` row, and the price of doing nothing is also zero. What "cheap now"
actually buys is the right to decide on the merits without weighing a migration path — so the
question to answer for each problem is only *is the new name better*, and any answer of "not
clearly" should resolve to leaving it alone.

## Consolidated change counts

Lines carrying the literal command string, whole repository as of `ef7164c`, `gen/` and this record
excluded.

| Command | Lines | Files | docs + README | examples | Action (`action.yml` + `scripts/`) | workflows | `deploy/` | Go |
|---|---|---|---|---|---|---|---|---|
| `godwit plan` | 72 | 33 | 51 | 3 | 1 | 2 | 1 | 14 |
| `godwit apply` | 80 (66 mean `migrate`) | 22 | 34 | 6 | 5 | 30 | 1 | 1 |
| `godwit migrate` | 44 | 17 | 35 | 4 | 0 | 0 | 3 | 2 |
| `godwit revert` | 38 | 16 | 29 | 3 | 1 | 4 | 0 | 1 |
| `godwit down` | 8 | 5 | 8 | 0 | 0 | 0 | 0 | 0 |
| `godwit target baseline` | 14 | 8 | 13 | 0 | 0 | 0 | 1 | 0 |
| `godwit target reconcile` | 19 | 11 | 15 | 0 | 0 | 0 | 1 | 3 |
| `godwit drift accept` | 11 | 9 | 7 | 0 | 0 | 0 | 1 | 3 |

Surfaces that **no** rename in this record touches: `godwit.yaml` **keys** (none is named after a
command; command names appear only in the *Used by* column, 8 rows), the Helm chart's
`values.yaml` and `templates/` (2 mentions total, both in `NOTES.txt`, both `godwit migrate`), and
every `rpc` in `api/proto/godwit/v1`. Renaming an RPC or an audit action string is a separate and
worse decision: `cp_audit.action` is `text` (`schema.go:152`) and rows already written keep the old
spelling, so a rename splits stored history in two.

## The evidence

**From this repository.** `internal/cli/commands.go:214` (`plan`, both paths), `:233`
(`engine.LoadDir`), `:263` (`target` as a `godwit.yaml` key), `:707` (`apply`), `:824` (`down`);
`internal/cli/service.go:149` (`migrate`), `:219` (`dryRun`, `persist` false), `:242`
(`persistPlan`, `persist` true), `:327` (`revert`), `:37`/`:70` (`baseline`, `reconcile`),
`:705` (`drift accept`); `internal/cli/commands.go:28` (`targetFlags.register`, the `--dsn` trio);
`internal/api/auth.go:50` (`PlanRun` is `ScopeRead`); `api/proto/godwit/v1` (`PlanRunRequest.persist`,
field 7); `internal/controlplane/audit_store.go:28`–`:35` and `internal/controlplane/schema.go:152`
(audit actions as stored text); `internal/ui/templates/drift.html:17`–`:19` (the button and its
note); `scripts/action-run.sh:317` (`apply` dispatches to `cmd_migrate`); `docs/ci-cd.md:37`;
`docs/configuration.md:22` (the `plan --target ""` paragraph), `:198` and `:216` (the same command
listed in the **Local** table and in the **Service** table); `docs/concepts.md:699` (*"Two ways
in"*).
[#105](https://github.com/SamuelMolling/godwit/pull/105),
[#106](https://github.com/SamuelMolling/godwit/pull/106).

**Counted for this record**, not estimated: every figure above is a `grep -rnE` over the working
tree with `gen/` excluded, over `*.md`, `*.yml`, `*.yaml`, `*.go`, `*.txt`, `*.sh`, `*.tpl`,
`*.html`. The `apply` split counts occurrences rather than lines, because four lines carry the word
twice.

**From the corpus.** Fetched pages are cited inline. Two claims rest on search summaries of official
documentation rather than a page read end to end, and are marked as such where they appear: Flyway's
`dryRunOutput` being Teams-only, and the exact wording of `atlas migrate set`. Neither is
load-bearing for any recommendation.

---

# The decision

*Everything above is analysis. This was the recommendation, and the owner took all four of it.
Two renames, two refusals.*

### 1. `plan` — **1b, taken. Add `--save`; drop `target` from `plan`'s `godwit.yaml` keys. Do not rename.**

The problem is not the verb. `plan` is the right word for both halves and the whole industry agrees
— what is wrong is that the durable, audited, DDL-executing half is the *default* and can be
switched on by a file the user did not read. `PlanRunRequest.persist` already exists and the CLI
simply never exposed it; 1b is closer to a bug fix than a rename. Terraform has spent a decade
teaching every engineer that `plan` is throwaway unless you ask for a file, and godwit's `migrate`
already re-plans when nothing matches, so the mental model transfers whole. Cost: one flag, one
config key removed, one deleted warning paragraph, `--save` added to one Action invocation, and
`--target ""` retired. Nothing else in the table moves.

The counter-argument, and it is decent: a flag does not stop `plan --target` from replaying your DDL
on a scratch database, so blast radius still changes with a flag rather than with a name. True, and
1c would fix it. I still take 1b, because the scratch replay is `migrate --dry-run`'s behaviour too
and nobody has found that surprising — what surprises people is the durable artifact, and that is
exactly what `--save` fences off.

### 2. `baseline`/`reconcile` — **2b, taken. One command: `godwit target adopt`, with `--version N` or `--from-journal`.**

This is the clearest rename in the record. `concepts.md` already found the parent concept and named
it; the CLI is the only surface that does not have it. The flag names the thing that actually
differs — where the truth comes from — so the choice stops requiring the reader to have memorised
two definitions written in terms of each other. Liquibase and Django both split this operation the
same way and both use one verb with two forms; **no tool in the corpus uses two unrelated names.**
It costs 33 lines across 17 files, touches no RPC, no audit string, no Action input, no Helm
template and no config key, and it frees `baseline` to mean exactly one thing.

### 3. `drift accept` — **3a, taken. Document. Do not rename.**

`accept` is the best word anybody in this space has produced for this operation; Terraform reached
for *record*, Flyway for *incorporate*, and Atlas did not ship the operation at all. The gap is that
the CLI is the one surface of five that states the consequence, and it states it only in `cli.md` —
the `Short:` help says "Bless the live schema as the new baseline", a verb used nowhere else in the
product, and the output line says `baseline accepted` and stops. Port the UI's own sentence into
both. Two Go lines. **A command whose consequence needs explaining is not misnamed; it is
under-documented at the point of use.**

### 4. `revert`/`down` — **4b, taken. Document only, but rename `godwit apply` to `godwit up`.**

The `revert`/`down` pair is fine and the local/service split behind it is enforced in code, not just
described: three commands take `--dsn`, everything else takes `--server`, and nothing crosses. What
the names fail to do is *announce* that split — and the fix is one rename, not four. `up`/`status`/
`down` against `migrate`/`target status`/`revert` makes the two worlds legible for the first time,
in the vocabulary golang-migrate, Rails and Alembic already taught, for **14 lines across 10 files**.

It also resolves a collision that documentation cannot: 66 of the 80 mentions of `godwit apply` in
this repository mean `godwit migrate`. Fixing that at the Action end costs a hundred lines and
breaks every consumer's workflow; fixing it at the CLI end costs fourteen lines and breaks nothing,
because **the Action never invokes `godwit apply` at all.**

### On aliases

**Add none.** Cobra makes it one line, nothing is released, and an alias would preserve exactly the
ambiguity each rename exists to remove.

### What would change my mind

- **On 1b:** a user planning against production with a `read` token and being surprised that they
  wrote to the store. That is 1c's case, and it is stronger than anything in this record if it
  actually happens.
- **On 2b:** evidence that people arrive at godwit already knowing Flyway's `baseline` and reach for
  it by name. Then 2c — keep `baseline`, rename only `reconcile` — is the better trade, and the
  collision with the drift baseline gets solved by wording rather than by the command tree.
- **On 3a:** a second person, independently, reading `drift accept` as "mute the alert". One
  documentation writer finding it hard to phrase is not evidence about users.
- **On 4b:** a decision to make the Action and the CLI share one vocabulary end to end. Then `apply`
  should mean the service path in both, `migrate` becomes the alias rather than the name, and this
  record's problem 4 reopens as a much larger question about what the Action is.

---

## What shipped

The owner's framing was *"a complete tool that is also simple to operate"*, and it is the tie-breaker
for the judgement calls the recommendation left open.

**1. `plan` — three forms, named in `--help`.** `--target` now reaches the service without storing
anything, which is the read-only form the record noticed was missing rather than a mode nobody
asked for: it is `migrate --dry-run` under the name a reader looks for. `--save` adds the durable
plan. `target` is gone from `plan`'s `godwit.yaml` keys, so no file switches the mode and
`--target ""` is retired; `--save` without `--target` is refused by name. The Action's `plan` step
passes `--save`, which is the one invocation in the repository that needed it.

**2. `target adopt`.** One command over `BaselineTarget` and `ReconcileTarget`, which keep their
names. `--version N` or `--from-journal`, exactly one required; neither and both are refused with a
message that states what each flag takes its truth from. `internal/api/plan.go`'s refusal now names
`godwit target adopt <t> --from-journal`, so the service and the CLI still ship together.

**3. `drift accept` — not renamed, documented at the point of use.** `Short:` lost "bless" and the
output gained the UI's own sentence, so the four surfaces agree: *records the current schema as the
new reference for the target; it does not change the database; future checks compare against it*,
plus the consequence `cli.md` already carried — no migration file describes the change.

**4. `apply` → `up`.** All 14 CLI-meaning occurrences were rewritten; the 66 that mean `migrate`
(62 `/godwit apply`, 4 `## godwit apply`) were left exactly as they were, which is what the rename
was for. `up`/`down` is the local pair, `migrate`/`revert` the service pair, and both `--help`
strings now say so.

**What the sweep turned up that the record had not.**

- **`docs/configuration.md` listed `plan` as taking `--dsn`.** It does not and never did: only
  `up`, `status` and `down` register that flag. Fixed in passing.
- **Two of the 14 `godwit apply` occurrences were never the CLI's command.** `decisions/0003` and
  `decisions/README.md` use it for a *declarative* `apply schema.sql` that godwit refuses to ship —
  a third meaning again. Renaming those to `up` would have misnamed the hypothetical, so the
  `godwit` prefix was dropped instead: the refused mode is `apply schema.sql`, and the word `apply`
  now belongs to the Action alone.
- **`godwit new` was missing from `docs/cli.md`.** Shipped in #105, never added to the reference
  written in #106. It has a section now, in the group for writing a migration.
- **`--save` changed two tests that were not about naming.** Anything asserting that a `CreateRun`
  binds to a stored plan has to store one first, which is the behaviour change the flag exists to
  make explicit.

**Shipped in** #109.
