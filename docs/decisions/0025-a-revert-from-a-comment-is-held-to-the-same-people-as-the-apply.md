# 0025 — A revert from a comment is held to the same people as the apply, and may not remove a gate

Shipped in #PR.

## The question

`revert` was the one destructive command a comment could command alone. `needsApproval` held `apply` and
`confirm`; `revert` was absent, and the manual asserted it without argument — *"`plan` and `revert` need no
approval; `revert` in particular is a way out, and requiring a review to take one would be the wrong
shape."* No decision record argued it.

The comment grammar accepted `--allow-data-loss` and `--force` from a plain comment, and `--allow-data-loss`
short-circuits the only gate there is: `if total == 0 || allow`. So one account with write permission could
comment `godwit revert --allow-data-loss --force` on a pull request whose apply two people agreed to, and
drop what that apply created — with no second person anywhere on the destroying path. The forward path
needed two; the path that destroys needed one.

## The decision

**`revert` joins `needsApproval`.** A destructive command from a comment is held to the same standing
approval as the command that created what it destroys.

**On its own that is a floor, not a fix, and it is worth being explicit about why.** The approval standing
on a pull request that has been applied is almost always the approval that permitted the apply — so
requiring one changes nothing about the common case. What it does close is the case where the apply never
went through the App at all (`require-approval: "false"`, a pipeline `godwit migrate`, a run created over
the API): the revert of such a run used to need nobody, and now needs the same person the App would have
needed to create it.

**The fix is the flags, because the flags are what make it destructive.** A comment may no longer carry
`--allow-data-loss` or `--force`; the forge policy refuses the command and says so:

> godwit revert refused: a comment may not remove a gate on what a revert drops (--allow-data-loss); that
> decision stays on the operator surfaces, where the credential is the target's own

An approval is consent to the migration, not consent to undo it over rows that were written since, and
repository write is not the credential those rows are behind. The two flags stay exactly where they were —
`godwit revert --allow-data-loss` on the CLI, the UI's revert form, the RPC — all of which need a token,
which by the [threat model](../run/security.md#threat-model-in-one-paragraph) is the target's own
credential. What is removed is the ability to reach them from a comment box.

**`revert` stays out of `wantsOpen`.** A pull request applied and then closed unmerged is precisely when a
revert is wanted; the merged case is already refused, and it is refused for the right reason — the
migrations belong to the base branch then. Requiring *open* would refuse the recovery and permit nothing.

## What it costs

**The way out is slower.** The minutes after a bad apply are what `revert` exists for
([0005](0005-revert-scoped-to-the-ledger.md)), and a plain `godwit revert` from a comment still works with
the approval that is already standing, which is the ordinary case. The two cases that are now slower are the
one nobody ever approved, and the one that drops rows — and the second is meant to be.

## Not fixed here

**The Action's own guards are unchanged.** `scripts/action-context.sh` refuses a merged pull request for
`revert` and asks for an approval for `apply` and `confirm` only, and it passes a comment's
`allow-data-loss` and `force` through. A repository whose workflow runs `command: revert` therefore still
has the single-person path this record closes for the App. The two surfaces authorise separately
([0007](0007-the-action-authorises-with-permission-and-approval.md) is the Action's record) and the Action's
guards are covered only by the event matrix in `.github/workflows/action-smoke.yml`; bringing it into line
is its own change, with its own test, and is deliberately not smuggled into this one.

## Rejected

- **Adding `revert` to `needsApproval` and stopping there.** It reads as the narrow fix and leaves the
  destructive lever exactly where it was: the same standing approval satisfies it, and the flags still pass.
- **Refusing the flags in the comment grammar** (`internal/comment`). The parser would answer *"godwit
  revert does not take --allow-data-loss"*, which reads as a typo rather than as a policy, and it would put
  an authorisation decision in the tokeniser. The grammar still parses both flags so that the policy can
  name them in the refusal.
- **A second approval, submitted after the run.** GitHub reports a review's state, not a useful ordering
  against a run godwit created, so the check would be inferred rather than read — the mistake
  [0007](0007-the-action-authorises-with-permission-and-approval.md) withdrew for the commit anchor.
