# 0019 — The App reads the migrations at the head, and the listing decides before the bodies

Shipped in #134 — `plan` only. The mutating commands and the run lifecycle are not built; where this record describes them it says so.

## The open question

[0016](0016-the-app-is-bound-to-targets-by-the-server.md) settled who may command the App and [0017](0017-the-repository-asks-for-a-plan-and-the-binding-permits-it.md) settled which projects a pull request touches. Both stop at the point where something has to run. The Action gets the migrations for free: a runner checked the repository out before godwit was invoked. The App has no checkout, no disk to put one on, and — deliberately, per 0016 — no business executing anything the repository ships.

So: where do the files come from, and what stops a repository from making the App do unbounded work by asking politely?

## The decisions

### The Contents API at the head, never a clone

godwit reads the migration directory over `GET /repos/{owner}/{repo}/contents/{dir}?ref={sha}`, one listing and then one blob per file, with the installation token already narrowed to the one repository the signed payload named.

A clone was the obvious alternative and is worse in every direction: it needs disk in a process that has none, it needs `git` in a distroless image, and it fetches every byte of a repository to read a directory of them. It also drags in a category of problem the App exists to avoid — a checkout is a place where a repository's own hooks and its `.gitattributes` filters get a say.

`{sha}` is the head the delivery was authorised against, not a branch name. The head having moved since is a real case and is handled below; a fork's head is already refused before any of this (0016).

### The limits are applied to the listing, before a single body is fetched

The directory listing carries every entry's name and size. `--max-files`, `--max-file-bytes` and `--max-migrations` are decided from that, so a directory over any of them costs one request rather than one per file, and a repository cannot spend the App's GitHub budget by committing a thousand large files.

The rule is not a second copy of the API's. `limits.Limits.CheckListing` takes names and sizes; the admission that a `PlanRun` request already went through (`checkFiles`) now calls it with the bodies' own lengths. One rule, two callers — so the App cannot refuse a directory the API would admit, and it cannot admit one the API is about to refuse with a less useful message.

A blob is fetched with `Accept: application/vnd.github.raw` and a hard byte limit: over it, the fetch fails. It does not truncate. A migration read short would be planned, reported and approved as SQL nobody wrote.

### A full page is a partial answer

The Contents API returns at most 1000 entries for one directory and says nothing when it truncated. This is the same shape as `/pulls/{n}/files` at 3000, and it gets the same answer as in 0017: **a listing that could be partial is refused, never read as a smaller set**. Concluding "these are the migrations" from a truncated directory would report a migration as absent when it is committed, and a reviewer would approve on that.

**What this costs, plainly:** a project whose migration directory holds 1000 or more entries — 500 migrations, since each is two files — is refused by the App even though `--max-migrations` admits 2000. That is a real ceiling and it is lower than the Action's, which reads a directory off a disk with no such cap. Nothing about it is silent: the refusal names the count and the API. The escape hatch, if a repository ever reaches it, is the Git Trees API, which returns the whole tree in one request *and* reports `truncated` honestly. It is not built because no directory here is near the ceiling and an unused code path is a liability.

### The command runs after the delivery is answered, in the process that accepted it

GitHub wants a webhook answered in seconds, and a plan builds scratch databases and replays a migration history. So the delivery is verified, authorised, resolved, recorded and answered `202`; the command itself is carried out afterwards by a small pool of workers, bounded by `--github-workers`.

The queue is in memory. **A replica that dies between answering the delivery and finishing the command loses that command.** The delivery id is already recorded, so a redelivery is dropped as a duplicate; the plan never appears and the check stays open. The recovery is to comment `godwit plan` again. This is acceptable for `plan` — nothing ran, and the second command costs a plan — and it is exactly the property that has to change before the mutating commands land, because "the run happened and nobody was told" is a different kind of wrong. The durable binding between a run and the pull request it came from is that work, not this record's.

A full queue is *not* a dropped command: `enqueue` runs inside the delivery's own transaction, so a queue with no room fails the transaction, leaves the delivery id unrecorded, and answers `500`. GitHub's redelivery is then a fresh attempt rather than a duplicate godwit would refuse.

### The App calls the service in the process, with a principal rather than a token

The App holds `*api.Server` directly and calls it with `api.WithPrincipal(ctx, cmd.principal)`. There is no bearer token for the App, nowhere for one to leak from, and no loopback listener to reach.

The scope decision is not skipped, it is made explicitly: every call goes through `api.Authorize(procedure, principal)` first, so a command whose scope does not carry a procedure is refused exactly as the interceptor would refuse it. `plan` carries `read` and no webhook can ever reach `RegisterTarget` (0016).

**What this gives up**, and it is worth knowing: the calls bypass the interceptor chain, so an App-driven plan does not appear in the access log, and it does not queue behind `--max-concurrent-diffs`. The second one matters — both build scratch databases on the same server. `--github-workers` (default 2) is the App's own bound, and it is *added to* the API's rather than shared with it, so a scratch server sized for `--max-concurrent-diffs` needs sizing for the sum.

### The head is read again before the command runs

Between the delivery being accepted and the worker picking it up, the pull request may have moved. godwit re-reads the head and abandons a command whose head is no longer the pull request's, silently: the push that moved it arrived as its own delivery and is being planned under that one. Reporting a plan for a commit nobody is looking at is worse than reporting nothing, and saying "the head moved" on a pull request that is already being re-planned is noise.

This is a different check from the one in 0016, which compares a comment's named sha against the head at *acceptance*. Both exist; this one closes the window the queue opens.

## What was refused

- **Cloning, or `git archive`.** Disk, `git` in a distroless image, and a repository's own filters getting a say.
- **Trusting the listing's own count.** There isn't one. The Contents API reports neither a total nor a `truncated` flag for a directory, which is why a full page has to be read as suspect.
- **A second plan report for the App.** The comment and the check carry what `godwit plan` renders on a laptop and what the Action posts — the same describers over the same response, moved into `internal/report` so both can reach them (#133). A separate vocabulary for the App would be two things to keep true.
