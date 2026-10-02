# 0026 — A bound belongs where the bytes arrive, and the scratch budget belongs to the service

Shipped in #PRNUM.

## The question

The API's bounds were built with the handler: `connect.WithReadMaxBytes(--max-request-bytes)` on the wire and
a concurrency gate as an interceptor. The GitHub App holds `*api.Server` and calls it in the process
(0019), and the UI does the same (0004). Neither passes a handler, so neither passed either bound.

Two holes, and they are not the same kind.

**No aggregate byte bound existed off the wire at all.** `limits.CheckListing` bounds the file count (5000)
and each file (4 MiB), never the sum; the sum was `WithReadMaxBytes` and nothing else. The App's reader caps
the directory at 1000 entries and fetches the bodies in parallel into memory, so an App-driven plan accepted
roughly 4 GiB where the API accepted 32 MiB — from a repository, over the internet, on a listener that is now
internet-facing.

**The scratch gate was counted per transport rather than per server.** It exists so a burst cannot exhaust
the scratch server's `max_connections` and disk. Built in `Handler`, it counted the API's calls only, so the
App's workers and the UI's reverts added uncounted scratch replays on top of its budget — which 0019 noted
and priced as "size for the sum", and which is the wrong shape: the thing being protected is one server, so
there should be one budget.

## The decision

**The aggregate bound is a `limits.Budget`, charged as bodies arrive, at each path's own ingress.** One rule,
one number, one refusal string, in `internal/limits`. `CheckListing` charges the listed sizes, so a directory
GitHub already says is too big is refused before a single body is fetched; `githubapp.bodies` charges each
body as it lands and cancels its siblings, so a listing that understated its sizes is refused part-way
rather than held whole. Accepting 4 GiB and then refusing it is the denial of service, not the defence
against it, which is why this is not a check at the end.

The enforcement point differs by path because the ingress differs — the wire reader for the API, the fetch
loop for the App — and that is the *only* thing that differs. A second copy of the rule is how this comes
back.

**The scratch gate moves onto `*api.Server`, entered by `Diff`, `PlanRun`, `CreateRun`, `RevertRun` and
`Checkpoint` themselves, and leaves the interceptor chain.** It is one gate per service, created once from
`Limits` on first use, so every caller — over the wire, from the App's workers, from the UI — queues in the
same budget, and no caller is counted twice. `--max-concurrent-diffs` now means what its description always
said: calls admitted at once, whoever made them.

## What this costs

`--github-workers` is no longer additive with `--max-concurrent-diffs`: an App-driven plan can now wait up
to 30 seconds for a slot and then be refused with `resource_exhausted`, where before it always ran. That is
the point — the scratch server was the thing without a bound — but a deployment that sized
`--max-concurrent-diffs` low on the assumption the App ran outside it should raise it rather than discover
the queue.

The gate is entered at the top of each procedure rather than around the whole call, so request decoding is
no longer inside it. Nothing allocates a scratch database before that point, so the budget still covers
everything it was there to cover.

## What was refused

- **Wrapping the service the App is handed.** It leaves `*api.Server` untouched and would have worked today.
  It also puts the bound on the caller rather than on the thing being protected: the next in-process caller
  to be wired up gets the hole back, silently, which is exactly how the UI came to skip the gate.
- **Re-checking the aggregate after the bodies are in hand.** The memory is already taken by then.
- **A separate App-side byte limit.** Two numbers that must agree is one number with a drift schedule.
