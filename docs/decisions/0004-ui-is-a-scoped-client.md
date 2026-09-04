# 0004 — The web UI is a scoped API caller, not a privileged surface

Shipped in #62 (token auth and scope enforcement), #63 (`/ui/diff`), #64 (plans pages), #65 (`ListTargets` and the target pages), #70 (backfill progress).

## The open question

The operator UI shipped with two flaws recorded in its own pull request, and the question was how to close them without inventing an account model.

**Flaw A — the scope interceptor never ran.** `ui.Handler.ServeHTTP` put a hardcoded `Principal{Name: "ui:" + user, Scope: ScopeOperator}` in the context and then called the `*api.Server` methods directly. In-process calls do not pass through the connect interceptor, so the procedure→scope map was never consulted: whoever reached `/ui` was an operator.

**Flaw B — one shared identity next to a named-token model.** The service has `[]api.Token{Name, Scope, Secret}`, but `/ui` only knew `--ui-user` / `--ui-password`, an identity with no declared scope.

## The decision

**The UI authenticates with the tokens that already exist, and every in-process call is authorised by the same map the interceptor uses.**

- The authorisation decision was extracted from the interceptor into `api.Authorize(procedure, principal)` — about fifteen lines and one new exported function — and `internal/ui` runs it before every in-process call. *Rejected: loopback HTTP from the UI to the API*, which was rejected once already for the right reason: it audits every UI action under the token's name instead of `ui:<user>`. *Rejected: duplicating the scope table in `internal/ui`* — two tables drift.
- Basic auth stays, because browsers speak it natively and it needs no login page, cookie or session store. A password matching **any token's secret** (username ignored, constant-time compare of digests) signs in as `ui:<token name>` with that token's scope. `--ui-user`/`--ui-password` remain the single-user fallback and gained `--ui-scope`, so the shared identity's power is declared rather than assumed.
- Templates render each action behind a `.Can` map keyed by **UI action**, not by procedure, so a new page adds a row and reads `.Can.<action>` without touching the call helper. A request posted around the page — `POST /ui/runs/<id>/resume` typed by hand with a read token — is refused with `403` and the interceptor's own message, and never reaches the service.

## Consequences to live with

- **Tokens alone now protect `/ui`.** This is a behaviour change: a service with `GODWIT_TOKENS` but no `--ui-user` previously served the UI wide open with operator rights. The `ui enabled without basic auth` warning is now emitted only when there is genuinely no way to sign in.
- **`read` gets a working `/ui/diff` page and a working button** (#63). `Diff` is a `read` RPC and correctly so — it writes nothing to any database and nothing to disk. That made the earlier blanket statement "`read` sees every page and can press nothing" untrue, so `docs/security.md` was changed to say what is actually true. The submit button is still gated on `.Can.diff` and `Diff` still has its `uiActions` entry, so the page degrades on its own if `Diff` ever needs a wider scope. *Rejected: leaving `diff` out of the map and rendering the button unconditionally* — it would make this the one page whose button is not explained by the map every other page uses.
- **`/ui/diff` supplies the migration files it cannot see from the store** (#77). Since #73 `Diff` refuses when the target records repeatable migrations and the request carries no files, and the page has no migration directory. It sends the `R__` pairs stored with the target's newest plan or with the run that last succeeded, and tells the reader which snapshot that was and how it disagrees with what the target recorded. *Rejected: rebuilding the bodies from the plan's `migrations`* — those are only the pending set, and since #76 can be withheld, so a repeatable already matching the target is missing from them; `cp_plan_files` holds the whole directory. *Rejected: a page-level flag asking `Diff` to skip the check* — the page's job is to find an honest source, not to switch off a refusal.
- **`/ui/diff` writes nothing and offers no download.** It prints the up and down SQL, the filenames to save them under, and a copy button; committing the pair is the author's move. A page that wrote files would be the service reaching into a repository it cannot see. It also says on the page — not only in the docs — that Prisma, GORM and Django schemas run client-side, because that is where someone would otherwise go looking for the button.
- **`ListTargets` fixed a rail that was scanning runs.** The UI derived its target list from the last 100 runs, so a target registered but never migrated was invisible and one whose runs aged out disappeared. `ListTargets` (`read`) answers from the control plane with two queries and **opens no connection to any target**, so it works while a target is unreachable. The nav's attention badge kept its meaning and is now counted over every run rather than the newest hundred — a fix that fell out.
- **The pending count on a target page comes from the newest *ready* plan, not from files.** The service holds no migration directory of its own. The stored plan *is* the reviewed answer to "what is left to apply", it links to the plan page, and the page says so and points at `godwit target status --dir` for the comparison against disk. *Rejected: having the UI invent a file set* — a second, unreviewed pending set next to the plan.
- **The plan detail page does not deduplicate hazards.** The run page's panel folds `H001` on four statements into one line because it is a summary; the plan page is the audit view, so two `CREATE INDEX` statements show two `H001` entries against the SQL that raises them.
- **Backfill progress is honest about its denominator.** `rows_total` is `pg_class.reltuples` read once — stale until the last `ANALYZE`, `0` on a table never analysed, and counting every row while the batch only touches rows the predicate still matches. So the page prints `~200,000` and `≈41%`, never a bare number, clamps the bar at 100%, and states where the number comes from. Rows written and batches committed are exact and the block says which is which. No rate and no ETA: deriving one honestly needs the moment the backfill started, and `Run.created_at` is not it.
- **Progress renders only for a `running` run.** `cp_runs.progress` is never cleared, so a settled run still carries the last statement it reported — a leftover, not something moving. A `Finish` that nulls the column would be tidier and has not been written.

## Refused or deferred

| Thing | Verdict | Reason |
|---|---|---|
| A login page, cookies, or an account model | refused | The named-token model already exists and browsers speak basic auth. A session store and a logout flow to avoid typing a token once. |
| Polling the plans pages | refused | A plan is immutable once stored; only its state and `superseded_by` change, and both are driven by an operator action elsewhere. The runs page polls because runs move on their own. A refresh link is honest here. |
| A new widget, a bundler or a new dependency for progress | refused | Six lines of CSS on the existing block/chip/note vocabulary, riding the 3-second poll the pages already run. A card or a banner is for something that needs a decision; this needs none. |
| Two status contexts for a two-phase apply | refused (#58) | A held apply sets `godwit/applied` to **pending**, not `success` or `failure`. The apply did what it was asked; nothing is wrong. `pending` keeps a required check unmergeable without claiming an error, and it is one context to configure in branch protection instead of two. |

## Where the design document was overtaken

- **A CSS custom property for the progress bar does not work.** `html/template` sanitises the value in CSS context and it comes out as `ZgotmplZ`. The width is an inline `style="width:41%"`, asserted in a test so a refactor cannot silently blank the bar.
- **Partials are throttled in the scheduler, not the engine.** The engine reports what happened; how often that is worth a write to the control plane is the scheduler's business, and a `pause`-less backfill can commit tens of batches a second.
- **`keep_old` is on the target summary** although the design did not list it: it is a registered per-target setting that had no surface anywhere, and the target page is where an operator looks for one.
- **`godwit targets` is top-level**, next to `runs` and `plans`, rather than `godwit target list` — the plural noun is already the convention for "list the things on the service". The CLI had no way to list targets at all before this.
