# Language spike

The step 2 write-up that chose Go, kept as a record. The project was then called Git Buffer and the CLI `gb`; the experiment code and `run.sh` it mentions were throwaway and are not in this repository.

## Recommendation

**Go**, with a firm split of responsibilities:

| Job | Use | Why |
|---|---|---|
| Read refs, objects, trees; build blobs/trees/commits in memory; tree diffs | go-git | Fast, no worktree needed, safe as a reader (E1, E6) |
| **Every ref write** (editor branches, `desired`, `applied/*`, notes ref) | `git update-ref` (`--stdin` transactions when several refs move together) | go-git's own CAS is unsafe (E1) |
| Merges | `git merge-tree --write-tree -c attr.tree=<integration branch>` | go-git has no 3-way merge; git runs our `.gitattributes` merge drivers exactly as users' tools will (E2) |
| Unresolvable conflicts | temporary `git worktree` + `git merge` + any `git mergetool`, then CAS | Works with any tool unchanged (E3) |
| Apply records | gb's own notes writer (go-git objects + `update-ref` CAS) | `git notes add` silently loses concurrent writes (E4) |
| Trailers | plain text in the message | git already parses them (E5) |

Rust with gitoxide is a credible runner-up (it passed the CAS test that go-git failed), but it would put a second merge implementation next to git's, and the spike already found one place where it behaves differently (E2). Go plus the git CLI keeps exactly one merge engine: git's.

The cost is a runtime dependency on `git` ≥ 2.42 (for `attr.tree`). On the target machines (Nix) that is free. A spawned git call costs about 2.5 ms (`rev-parse`) to 5 ms (`update-ref`). A whole `gb capture dconf` took 8 to 15 ms.

## Results

All numbers come from `run.sh` in this folder (git 2.43, go-git v5.19.3, gix 0.89, Go 1.26, Rust 1.97). Every result below was reproduced on a second full run.

### E1. Compare-and-swap ref updates, 4 processes × 100 updates

Each worker reads `applied/test`, writes a commit on top of it, and CAS-moves the ref. Correct CAS means the chain length equals the number of successful swaps, and no reader ever fails.

| Writers | Lost updates | Reader anomalies | Verdict |
|---|---|---|---|
| git CLI only | 0 | 0 | OK |
| go-git `CheckAndSetReference` only | 0 | 400-490 "reference not found" | **broken** |
| go-git + git CLI mixed | **16 of 400** | 91 | **broken** |
| go-git reads, `git update-ref` writes ("hybrid") | 0 | 0 | OK |
| hybrid + git CLI | 0 | 0 | OK |
| gitoxide only | 0 | 0 | OK |
| gitoxide + git CLI / hybrid | 0 | 0 | OK |

Cause: go-git locks the ref file with `flock` and truncates it in place. git uses `<ref>.lock` plus an atomic rename. So go-git readers see an empty ref mid-write, and go-git and git do not exclude each other at all. Any user running `git` against the repo would race gb. gitoxide implements git's lock-file protocol and interoperates cleanly.

`git update-ref --stdin` with `start/prepare/commit` is atomic across refs. A transaction that moves `desired` and fails on a second ref leaves `desired` untouched (checked).

### E2. Three-way merge with a format-aware driver, bare repo

Two editors change adjacent lines of `vscode/settings.json` (`fontSize`, `tabSize`). A plain line merge conflicts. With a `gb-json` key-level driver registered through `.gitattributes`:

- `git merge-tree --write-tree` (no worktree) runs the driver and merges cleanly: `{fontSize: 14, tabSize: 2}`.
- **Finding:** in a bare repo, git reads `.gitattributes` from **HEAD's** tree, not from the trees being merged. With HEAD unborn or pointing elsewhere the driver silently doesn't run, and the result is a plain line conflict. `-c attr.tree=refs/heads/<integration>` fixes it. This matters because the design says gb never relies on HEAD/default-branch status, so gb must always pass `attr.tree` explicitly.
- **Finding:** the driver itself is registered in git config (`merge.gb-json.driver`), which is not cloned. `gb init` has to set it on every clone. Without it, git falls back to a line merge, which conflicts rather than producing a wrong result, so this is safe.
- go-git: fast-forward only (`Merge` returns `ErrUnsupportedMergeStrategy` for anything else).
- gitoxide `merge_commits`: runs the configured driver and merges the clean case correctly. It also reads attributes from HEAD (via the index), so it has the same pitfall. **But** when the driver exits 1 (git's protocol for "conflict, markers left in %A"), gix aborts the whole merge with an error instead of recording a conflict. That's a semantic difference from git that gb would have to work around.
- git2/libgit2: not built in this spike. libgit2 is documented not to run external merge drivers from config, which rules it out for the "any merge driver works" decision.

### E3. Handing a conflict to any mergetool

A real conflict (`fontSize` 14 vs 16) leaves the editor branch unintegrated. Then: `git worktree add --detach` on `desired`, then `git merge edits/c` (conflicts), then `git mergetool --tool=<anything>`, then commit, then `update-ref desired <new> <old>`. A scripted tool stood in for meld/VSCode. The result is a normal two-parent merge commit on `desired`. Nothing gb-specific is needed beyond the temp worktree and the final CAS.

### E4. Apply records as notes (`refs/notes/buffer-applied`)

- Notes written from go-git (plain blob/tree/commit) are read by `git notes show`, and the reverse also works, including git's fanout layout after 300 notes. go-git has no notes API, but it's about 40 lines.
- **Finding:** 4 concurrent `git notes add` processes writing 48 notes kept only **18 and 29** (two runs), with every process exiting 0. `git notes` does not CAS its ref. gb's own writer (go-git objects + `update-ref` CAS + retry) kept 48 of 48. So gb must never shell out to `git notes add` for apply records.

### E5. Trailers

`Buffer-Editor`, `Buffer-Adapter`, `Buffer-Session` and `Buffer-Keys` written by go-git are read by `git log --format=%(trailers:key=Buffer-Editor,valueonly)` and findable with `git log --grep`. No library support is needed.

### E6. dconf snapshot capture (`gb capture dconf`)

The run uses a real dconf daemon on a private D-Bus session. `dconf dump /` is split into one INI per dconf dir with sorted keys, built into a tree in memory, and compared with the branch tip by tree id.

- First capture: 1 commit, 3 keys, 15 ms. A capture with no change: no commit (identical tree id), 8 ms.
- Toggling one key gives one commit, `dconf: set /org/gnome/desktop/interface/color-scheme`, with a one-line `git diff`.

### E7. Concurrent captures

After one change, 4 capture processes started at once produced exactly 1 commit in all 6 runs, with or without the per-repo `flock`. The ref CAS alone prevents double commits. The lock still matters for its real job in the design, which is keeping capture and apply from interleaving. An `flock` is released automatically when the process dies.

## Rust vs Go, other factors

| | Go (go-git + git CLI) | Rust (gitoxide) |
|---|---|---|
| Ref CAS | via `git update-ref` | native, correct |
| Merge with drivers | git itself | native, but the driver-conflict semantics differ |
| Notes | hand-rolled (40 lines) | hand-rolled (no notes API either) |
| Clean release build | seconds | 87 s (163 crates) |
| Binary | 10 MB | 8 MB |
| Adapters later | godbus, fsnotify, client-go | zbus, notify, kube-rs |
| Runtime deps | git ≥ 2.42 | none |

Minor: go-git v5.19.3 requires Go 1.26 (the toolchain auto-downloads; nixpkgs has it).

## Impact on the design doc

No changes to the model. Four implementation rules come out of the spike for step 3:

1. gb never writes a ref through go-git. All ref moves go through `git update-ref` (transactions for multi-ref moves).
2. Every merge passes `attr.tree=<integration branch>`, and `gb init` registers the merge drivers in repo config.
3. Apply notes are written by gb with CAS, never by `git notes add`.
4. CAS is enough for capture/capture races. The per-repo `flock` serializes capture against apply.
