# Decision records

Why godwit is shaped the way it is. One record per question that was genuinely open, with the decision taken, the evidence behind it, what it costs, and what was refused.

These are **not** specifications. The rest of `docs/` describes what the code does; if a record and a page disagree, the page is the description and the record is the reason. If a record and the code disagree, the code wins and the record is stale — say so in a pull request rather than quietly editing the reasoning to match.

Each record names the pull requests that implemented it, so `gh pr view <n>` gives the implementation detail this level deliberately leaves out.

| # | Decision | Shipped in |
|---|---|---|
| [0001](0001-plan-as-contract.md) | The plan a reviewer approved is the contract the deploy applies | #39–#44 |
| [0002](0002-directives-godwit-executes.md) | `-- godwit:` directives: the migration declares intent, godwit writes the lock-safe SQL | #51–#53, #56–#59, #69 |
| [0003](0003-orm-schema-sources.md) | The ORM model is a desired schema, rendered client-side, gated by `lint` | #46, #48, #55, #60, #61 |
| [0004](0004-ui-is-a-scoped-client.md) | The web UI is a scoped API caller, not a privileged surface | #62–#65, #70 |
| [0005](0005-revert-scoped-to-the-ledger.md) | `revert` undoes what a run applied, read from the ledger, never the directory | #68, #71 |
| [0006](0006-repeatable-objects-are-desired.md) | What a repeatable declares is part of the desired schema | #73 |

## Open questions

`open/` holds questions that are argued but **not decided**. They are not records: nothing in them
binds the code, and they carry no number until a decision is taken, in either direction.

| Question | Proposes |
|---|---|
| [Renaming columns and tables](open/renaming-columns-and-tables.md) | teach the safe path and fix `godwit diff`; do not build a `rename-column` directive |

## Standing constraints these records assume

- **The journal is the truth.** Progress lives in the target database, committed with the DDL. Every mechanism below either uses it or explains why it cannot.
- **What runs is a versioned SQL file that went through the gate.** `godwit diff` writes such a file; it never replaces the history. A declarative `godwit apply schema.sql` stays refused.
- **The reviewer sees the exact SQL before it runs.** This is what rules out expanding directives in the scheduler, and what makes the plan a contract rather than a preview.
- **Roll forward is the production answer.** Down files are required as a review artifact; `revert` is for the minutes after a bad apply.
