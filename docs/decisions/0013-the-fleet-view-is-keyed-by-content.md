# 0013 — The fleet view is keyed by content, and answers rather than refuses

Shipped in #95.

## The open question

godwit knew, per target, exactly which migrations stood on it: the ledger (`0005`) is authoritative and
`GetTargetStatus` reads the target's own journal. Nothing ever crossed two targets. Answering *what is in staging that
is not in production yet* — the question anyone running the same migrations against two databases asks weekly — meant
reading two `target status` outputs side by side and comparing version lists by eye.

Two things had to be decided: what the unit of comparison is, and what the view is allowed to do with the answer.

## The decision

**One entry per migration and per content.** The key is the migration id (`<version>_<name>` or `R__<name>`) together
with the sha256 of the up file the target applied. A version two targets applied from different files is therefore two
entries, both flagged `divergent`, and each target reads *differs* in the other's row.

*Rejected: keying by version alone, with the checksum as a column.* It answers the easy half of the question and buries
the half that matters. Under that key `20260901_x` is one row that both targets "have", the disagreement is a detail
one column to the right, and — worse — `--in staging --not-in production` reports nothing, because production does have
*a* `20260901_x`. The whole reason to compare two environments is to find where they stopped meaning the same thing.

Repeatables need no special case: name and content is already their identity (`0006`), so the same key covers them.

**Standing is the ledger's predicate, reused.** A migration is on a target when its `cp_run_applied` row is not `held`
and no revert withdrew it — the same clause `Applied`, the replay and the out-of-order guard use. A run that applied
three migrations and then failed on the fourth shows those three here, exactly as the target's journal has them. There
is no second definition of "applied" anywhere in godwit and this view did not add one.

**A target that lacks a migration is reported with the reason.** *not there yet* (its newest standing version is below
this one), *missing* (it is past this version and does not have it) and *differs* (it has it under other content) are
three different situations, and only the middle one is alarming. A migration a checkpoint collapsed (`0010`) names the
checkpoint that recorded it, because on a database built from that checkpoint it never ran. A migration whose file
bodies retention swept keeps its row with the content `unknown` rather than disappearing — dropping it would state that
the target does not have it, which is the one thing the view must never get wrong.

*Rejected: a bare boolean per target.* "production does not have it" is true of a version written yesterday and of one
deliberately skipped, and an operator cannot act on the first reading without the second.

**The view is read-only. There is no promotion gate.** It does not refuse a run in production because staging never saw
the migration, and nothing in the service consults it before admission.

*Rejected, for now: gating `CreateRun` on a predecessor environment.* Not because it is wrong — it is the obvious next
step — but because it is a policy decision the owner has not taken, and every part of it is a question this change does
not need to answer: which target is upstream of which (godwit has no notion of an environment order), whether the gate
compares versions or checksums, how long staging must have held the migration, what the break-glass is and who may
press it, and what happens to a hotfix that must reach production first. Shipping the view first means those questions
get argued against a screen that already shows the fleet, rather than in the abstract.

## Consequences to live with

- **Divergence is loud, and it is loud twice.** A version standing under two checksums produces two rows and each shows
  *differs* on the other target. That is deliberate: the row you happen to be looking at is not allowed to look normal.
- **The view is only as good as the ledger.** It says nothing about a database godwit does not manage, nothing about
  drift inside a migration (that is `CheckDrift`), and it does not read any target's journal, so a target whose journal
  was edited by hand disagrees with it until the next `godwit target status` or drift check says so.
- **The answer grows with the fleet, not with the run history.** It is two queries over `cp_run_applied` and
  `cp_targets`, keyed by the newest standing row per target and migration, so a target with a thousand runs and a target
  with three cost the same to compare.
