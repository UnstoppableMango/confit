# Git Buffer: design and constraints

The step 1 design, kept as written. The project was then called Git Buffer; it is now confit. What was built differs in places, see the README.

## 1. What it is

Git Buffer sits between a UI that edits configuration and the system that configuration lives on. Every edit made in a UI becomes a git commit. Whether the edit also reaches the system right away depends on the adapter. What is non-negotiable is that drift between the system and git is always detected and recorded (§4, Drift). Something else (a "consumer", e.g. home-manager, NixOS, kubectl) applies those commits later and records that it did.

Two kinds of pointer carry all the state:

- **Edit pointer**: where a UI has edited up to.
- **Applied pointer**: what a consumer has actually applied.

Everything else (pending changes, history, blame, rollback, review) is derived from git.

## 2. Use-cases driving the design

| # | Editor (UI) | Where edits normally go | Consumer | Repo format |
|---|---|---|---|---|
| U1 | VSCode settings UI | `~/.config/Code/User/settings.json` (file write) | home-manager (`programs.vscode.userSettings`) | JSON/JSONC |
| U2 | GNOME Settings, Tweaks, any GSettings app | dconf database via the `ca.desrt.dconf` D-Bus writer | home-manager `dconf.settings` / NixOS | dconf keyfile (`dconf dump` INI) |
| U3 | "Any UI editing a system" | file, D-Bus, HTTP API, ... | anything | per adapter |
| later | k8s UIs (Lens, Headlamp, dashboards) | Kubernetes API server | kubectl / Flux / Argo | YAML/JSON manifests |

**Does this change the plan?** Yes, the first target moves. Both concrete use-cases are desktop config on a Nix machine, not Kubernetes. Proposed change:

- Step 3 builds the core against **JSON files (U1) and dconf (U2)** instead of k8s manifests. JSON covers most of what k8s needs anyway; k8s becomes the third format.
- Step 4's end-to-end run uses **home-manager as the consumer** instead of kubectl against kind.
- Step 2's spike adds one item: **capturing dconf changes and reconciling snapshots** (see §5).

The core (refs, pending, metadata, merging) is unchanged by this; only the adapters and the first formats differ.

## 3. Ref model

Plain branches under fixed namespaces, so every git tool (log, diff, mergetool, GitHub, Nix flake inputs) can see them:

```
refs/heads/edits/<editor>      one per editor instance (e.g. edits/vscode@laptop, edits/dconf@laptop)
refs/heads/desired             the integrated intent: editor branches merged together
refs/heads/applied/<consumer>  last commit a consumer fully applied (e.g. applied/home-manager@laptop)
```

**Branch names are configuration, not convention.** The integration branch defaults to `desired`, but it can be renamed (e.g. `main`, so that it is the repo's default branch and what consumers clone by default). The tooling only knows the configured name. It never reads or relies on which branch is the remote's default (`HEAD`, `init.defaultBranch`, GitHub's default branch). The `edits/` and `applied/` prefixes are configurable the same way. Config lives in the repo (`git config gitbuffer.integrationBranch main`, or a committed `.gitbuffer` file) so every editor and consumer agrees. The rest of this doc says `desired` to mean "the configured integration branch".

Rules:

1. **Editors only write their own branch.** An editor commits on top of its branch. Before committing it rebases or merges from `desired` so it builds on what everyone else intended.
2. **Integration** moves `desired` forward by merging editor branches (fast-forward when possible). It runs at the end of each `gb capture`, or on demand. A conflicting editor branch is left unmerged and reported (§6); it never blocks other editors.
3. **Each consumer follows one source ref**, `desired` by default. A consumer can follow an editor branch directly (single-editor setups) or a curated ref (e.g. `release`) if someone wants review before apply.
4. **Applied refs move only by compare-and-swap** (`git update-ref applied/X <new> <expected-old>`). That gives safe concurrency across processes with no daemon or lock server.
5. Applied refs only move to commits reachable from the consumer's source ref. Applying something that isn't in history is an error.
6. **Drift**: any live change not yet in git is committed (to the adapter's branch, or `edits/drift@<host>` when it was found by reconcile) and then handled by the consumer's drift policy. See §4, Drift.

n editors × n consumers falls out naturally: editors fan in to `desired`, and consumers fan out from it, each with its own pointer.

## 4. "Pending"

For consumer `C` following source ref `S`:

- **pending(C)** = `applied/C..S`, the commits in the source not yet applied, plus the tree diff `diff(applied/C, S)`.
- A consumer applies the **tree at a commit**, not a sequence of patches. Config is declarative, so applying the final state is correct and idempotent; the commit list is for humans and audit.
- After a successful apply, the consumer CAS-advances `applied/C` to exactly the commit whose tree it applied. A partial failure leaves the pointer where it was.
- `pending` is empty when `applied/C` == `S`. "Behind but nothing to do" (only no-op commits) is reported as pending commits with an empty diff.

Edits still sitting in an editor branch and not yet integrated are a separate state, **unintegrated(E)** = `desired..edits/E`. A conflict shows up here.

### Drift (hard requirement)

Edits are allowed to reach the live system. The hard requirement is that **every difference between the live system and git is detected, recorded and attributed**, and then handled by an explicit policy. Nothing is ever lost or overwritten silently.

Invariants:

1. **Everything lands in git.** Every change to live state becomes a commit within a bounded time. This covers changes made through a UI, by hand, by another tool, or while Git Buffer wasn't running.
2. **Every commit is attributed.** It records whether it came from a known editor (`Buffer-Editor: vscode@laptop`), from an unknown source (`Buffer-Source: unattributed`), or from a consumer apply.
3. **Consumers never apply blind.** Before applying, a consumer checks drift. It takes a snapshot of live state and compares it with what git says should be there. Any change not yet recorded is committed first, and only then does the apply go ahead.

How drift is detected (each adapter implements both):

- **Snapshot capture is the one mechanism.** `gb capture <adapter>` is a one-shot command. It reads the full live state into a tree, diffs it against the expected tree, commits any difference, runs integration, and exits. It covers UI edits, offline edits, and anything that happened while nothing was watching. Because it always compares whole states, there is no event log to lose and no "missed event" case.
- **Triggers decide latency, not correctness.** The same command runs from whatever is cheapest: before every apply (always), at login, from a systemd timer, from a systemd `.path` unit for file-backed systems (no daemon of ours), or by hand. A capture that runs late is still correct.
- **Watchers are optional and tiny.** Where nothing existing can trigger on change (dconf has no file-level signal worth trusting), a watcher's only job is to wait for a change, debounce, and exec `gb capture <adapter>`. It holds no state and has no git logic, and killing it loses nothing except latency.
- **No echo suppression is needed.** After an apply, the live state equals the applied tree, so the next capture finds nothing. Capture and apply take the same per-repo lock (or retry on a failed CAS), so they never interleave.

Drift policy, configured per consumer:

| Policy | What happens to drift | Use when |
|---|---|---|
| `adopt` (default) | The drift commit is integrated into `desired` like any edit. The next apply reproduces it. | Desktop config. A change made in GNOME Settings survives `home-manager switch`. |
| `revert` | The drift is recorded for audit, then the apply restores `desired`. | Systems where git must win (most k8s). |
| `block` | The consumer refuses to apply until a human adopts or discards the drift. | High-stakes systems. |

## 5. Capturing edits from existing UIs

Since edits may reach the system, the default approach is **pass-through and capture**: the UI writes to the system as normal, and Git Buffer records the change. True interception is optional, used only when an adapter can do it cheaply.

| Pattern | How | Notes |
|---|---|---|
| **A. Pass-through + capture** (default) | The UI writes normally. `gb capture` turns the change into commits. | No hooks into the UI and no read-your-writes problem. This is the same mechanism as drift detection, so one mechanism covers UI edits and out-of-band changes alike. |
| **B. Redirected file** | The file the UI writes is a file in a Git Buffer worktree. | Useful when the real file is read-only (Nix store) and something has to own a writable copy. |
| **C. Write proxy** | Take over the write channel and record writes without forwarding them. | Only where "doesn't reach the system" is genuinely wanted. Must also serve reads (see the dconf note). Not in v1. |

Per use-case:

- **U1 VSCode → B, captured like A.** Under home-manager, `settings.json` is a read-only Nix store symlink, so it gets replaced by a writable file in the worktree. A systemd `.path` unit runs `gb capture vscode` when the file changes, so no Git Buffer daemon is involved. Saves land there (live for VSCode) and get committed. home-manager reads the committed version. The file in the worktree is the live state, so drift is anything in it that hasn't been committed yet.
- **U2 dconf → A.** Capture uses `dconf dump /` against the repo's INI files. For low latency, an optional small watcher (`dconf watch /` piped into a debounce that execs `gb capture dconf`) can trigger it. Without the watcher, captures run on login, a timer, and before apply. Interception isn't needed any more, which removes the earlier read-your-writes problem: dconf reads come straight from the database file, so a write proxy would have made the UI snap back. Attribution is the weak spot, because dconf doesn't say which app wrote a key. Changes are attributed to `dconf@host` unless a richer source is known.
- **U3 generic.** The adapter contract is (a) `snapshot` → tree and (b) an optional `apply(tree)` for consumers that use the same adapter. Both are one-shot. An adapter can be built into `gb` or be an external executable (`gb-adapter-<name> snapshot|apply`, in the style of git's own helper commands), so each can be written in whatever fits its system. A change trigger is optional and lives outside the adapter.

**Commit granularity.** UIs are chatty: one toggle can mean one write, and a slider drag can mean dozens. Adapters debounce, committing after a quiet period (default ~2s), with an option to squash a session's commits on integration.

## 6. Repo layout, formats and conflicts

One repo per machine/user scope, with one directory per domain:

```
vscode/settings.json
dconf/org/gnome/desktop/interface.ini     # one file per dconf dir, `dconf dump` format
k8s/<cluster>/<ns>/<kind>/<name>.yaml     # later
.gitattributes                            # maps paths to merge drivers
```

- **Stable serialization** keeps diffs and merges small: one dconf dir per file with sorted keys, and k8s with one object per file. VSCode's `settings.json` is JSONC (comments, trailing commas), so the adapter preserves the user's formatting rather than canonicalizing it. Rewriting it would destroy comments.
- **Merging is plain git merge, with format-aware merge drivers registered through `.gitattributes`.** This is the key decision for "any conflict tool works". Git Buffer's resolver is just a merge driver. Anyone can swap it for their own, or fall through to `git mergetool`.
- Built-in drivers resolve the common cases automatically:
  - JSON/YAML: different keys changed → merge; lists of objects keyed by `name` (k8s containers, env) → merge per item.
  - dconf INI: per-key merge (line-based merge mostly does this already).
  - Same key changed to different values → a real conflict, written with normal conflict markers.
- **Unresolved conflicts** leave the editor branch unintegrated. `gb status` reports it, and the user resolves it with any git tool on a checkout of `desired` merging `edits/E`, then integration continues.

## 7. Commit metadata

- **Author** = the human (from git config or the session user). **Committer** = Git Buffer + adapter name.
- **Message** = a human summary generated by the adapter ("vscode: set editor.fontSize = 14").
- **Trailers** (machine-readable, survive rebase, queryable with `git log --format=%(trailers)`):
  ```
  Buffer-Editor: vscode@laptop
  Buffer-Adapter: file-watch/0.1
  Buffer-Session: 7f3c...        # groups commits from one UI session
  Buffer-Keys: editor.fontSize   # what changed, when the adapter knows
  ```
- **Apply records**: when a consumer advances its pointer, it attaches a git note (`refs/notes/buffer-applied`) to the applied commit with the consumer, time, result and tool version. Reflogs aren't pushed, so notes are what make apply history portable.

## 8. CLI vs library: the constraints that decide it

| Constraint | Implication |
|---|---|
| Consumers are arbitrary (Nix, shell, kubectl, CI) and written in any language | A **CLI with a stable, machine-readable (`--json`) interface** is the universal contract. |
| All state is in git refs and CAS updates are atomic | **No daemon is needed for correctness.** Any process can be an editor or consumer. |
| Minimize daemons; any we keep must be small and focused | Every operation is a **one-shot command**. Triggering uses systemd units, existing tools, or a tiny stateless watcher that only execs `gb capture`. In-tree adapters link the core **library**; external ones are helper executables. |
| Editors can live inside other runtimes (a VSCode extension is TypeScript) | They shell out to the CLI. A native binding isn't needed at first. |
| Nix integration wants pure evaluation | Consumers read from git (flake input on `desired`). Git Buffer only needs to advance `applied/*` after `switch` succeeds: `gb applied advance home-manager@laptop <commit>`. |
| Must work with any git tooling | Use only standard objects, branches, notes and `.gitattributes`. No custom object formats and no hidden state outside the repo. |

**Result:** a core library with a thin CLI (`gb`) on top. The CLI is the public contract, the library is an implementation detail for in-tree adapters, and nothing requires a server.

Sketch of the CLI surface:

```
gb init
gb edit commit <editor> [--session ID]     # used by adapters; reads worktree changes
gb integrate [<editor>...]                 # merge edits/* into desired
gb pending <consumer> [--json]             # commits + diff since applied/<consumer>
gb applied advance <consumer> <commit> [--expect <old>]
gb status [--json]                         # unintegrated, conflicts, pending per consumer, drift
gb capture <adapter>                       # snapshot live state, commit drift, integrate (one-shot)
gb watch <adapter>                         # optional tiny trigger: on change, debounce, exec capture
```

## 9. Language (input to step 2)

Go is still the recommendation (single binary; fsnotify and godbus for the two adapters; client-go for k8s later). One risk to check in the spike is that **go-git has weak merge support** (no general three-way content merge). The spike should confirm which of these holds up:

- go-git for refs, CAS, objects and notes, shelling out to `git merge` / `git merge-file` for merges (merge drivers run inside git anyway, so this may be the natural fit), or
- libgit2 via git2go, or Rust with gitoxide (`gix-merge`) if shelling out turns out to be a problem.

Spike checklist: CAS ref update under concurrent writers, three-way merge with a custom driver, notes read/write, dconf snapshot capture via `dconf dump`, and capture/apply locking (§4, §5).

## 10. Open questions for Erik

1. **One repo per machine, or one repo shared across machines** with `@host` in branch names? The design allows both. Shared makes home-manager configs portable, but it means pushing to a remote.
2. **Should `desired` integrate automatically**, or do you want a review gate (e.g. consumers follow `release`, which you advance by hand)?
3. Is **`adopt`** the right default drift policy for desktop config, so out-of-band changes flow into `desired` automatically?
4. Out of scope for v1, confirm: secrets in config, Windows/macOS, and multi-user shared repos with permissions.
