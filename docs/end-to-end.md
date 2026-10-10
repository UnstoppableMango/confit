# Step 4: end to end on the real use-cases

Step 4 of the plan runs confit against the two use-cases from the design (U1 and U2), with home-manager as the consumer. This page covers what ran, what it found, and what is still open.

## What ran

Two scenarios run in CI on every PR (the `e2e` job, which feeds `required`). Both also run locally.

**`e2e/e2e_test.go`** (`make test-all`) drives the real `confit` binary, real git and a throwaway dconf database on a private D-Bus:

| Step | What happens | Checked |
|---|---|---|
| First run | `confit watch` captures an existing `settings.json` (with comments and a trailing comma), and the first apply records it | `desired` holds it unchanged, `applied/` = `desired` |
| VSCode settings UI | `settings.json` is saved in place, and then by write-and-rename (`files.saveAtomically`) | One commit each: `vscode: set editor.fontSize = 16`, comments kept |
| GNOME Settings / Tweaks | `gsettings set org.gnome.desktop.interface color-scheme 'prefer-dark'` | `dconf: set /org/gnome/desktop/interface/color-scheme = 'prefer-dark'` |
| Slider drag | 20 `text-scaling-factor` writes in about 1s | Exactly one commit |
| Edit in git | A commit to `desired` from a plain git worktree, then `confit apply` | Reaches settings.json and dconf, and the watchers' captures don't echo it back |
| Offline drift | Changes while no watcher runs, then a switch | Captured and adopted before the apply, so they survive |
| Conflict | A UI edit vs. a committed but unapplied edit of the same key | Reported by `status`. The apply refuses (exit 3) and leaves the live file alone. Resolved with `git merge` + commit, then applied. |
| `revert` | A live change, then a switch | Restored to `desired`, and the drift is still on the editor branch |
| `block` | A live change, then a switch | Refuses until `confit integrate` keeps it, and the next switch then reproduces it |

Where GSettings with the GNOME schemas is available, the test uses `gsettings`, the same path GNOME Settings and Tweaks take. In the Nix dev shell it writes the same keys with `dconf write` instead, and logs that it did.

**`e2e/home-manager/`** (`make e2e-home-manager`) builds a real home-manager generation with the new module (`services.confit`, with VSCode and dconf enabled, plus one key in `dconf.settings`) and runs real `activate` switches in a throwaway HOME:

1. First switch on an existing desktop: the repo is created, the live state is captured, and `applied/home-manager@e2e` = `desired`.
2. A VSCode edit and two Tweaks changes are captured the way the systemd units run them (`confit capture vscode` from the `.path` unit, `confit watch dconf` from the service). The two Tweaks changes become one commit.
3. A change made while nothing watched is adopted by the next switch, and an apply note is written.
4. Tweaks changes `button-layout`, which home-manager also declares. The switch records the UI change first, and then home-manager's value wins.
5. A switch with nothing new moves no branch.

## Bugs it found (fixed in this PR)

1. **`block` couldn't be cleared by integrating.** Both the README and the error message say to integrate the drift to keep it, but the next apply stayed blocked. Integrated drift now counts as accounted for.
2. **Editor branches could lose their link to applied history.** An editor branch captured before the first apply (under `revert`/`block`) never got its sync commit, because the content was equal. Its next change then merged against the root and conflicted with itself. The sync now also checks ancestry.
3. **A hand-edited dconf file came back as an `update` commit.** A key added to `desired` by hand, not in sorted order, was rewritten by the next capture. Captures now keep the committed bytes when the keys and values are equal.
4. **A switch overwrote a UI change to a key in `dconf.settings` without recording it.** home-manager's `dconfSettings` step ran before confit could capture it. The module now captures before `writeBoundary`, and applies after `dconfSettings`.

## What was added

- `confit watch <adapter> [--quiet 2s]` follows `dconf watch` (dconf) or inotify on the files' directories (file). It captures once at start and again after each quiet period, by running `confit capture` as a separate process, so it holds no state.
- The `homeManagerModules.default` flake output (`nix/hm-module.nix`):
  - Every switch captures, then runs `confit apply`. A block or conflict is a warning, not a failed switch.
  - A `.path` unit per file adapter (no process of ours), `confit-watch-dconf` for dconf, and an hourly capture timer.
- The e2e harnesses above and the CI job.

## Not verified here

- **The VSCode GUI itself.** The tests write `settings.json` the two ways VSCode saves it. They don't run VSCode.
- **The systemd units under a running user manager.** The container has no systemd user session. The generated unit files were built and inspected, and their commands run exactly as the units would run them. The first real login on your machine is the remaining check.

## Decisions for you

1. **home-manager applies by running `confit apply` during activation**, not by reading the repo as a flake input. A pure `programs.vscode.userSettings` makes `settings.json` a read-only store link, so the settings UI can't save, and that defeats U1. The cost is that the switch isn't pure for these files. The module warns against setting both.
2. **Keys declared in both `dconf.settings` and a UI:** Nix wins on every switch, and both values are recorded. The alternative is to drop such keys from `dconf.settings` and let confit own them.
3. **History is two commits per capture** (the edit, then `Integrate edits/... into desired`). Editor branches never take `desired`'s merges, so integration can't fast-forward. Basing a new edit on `desired`, when the editor's subtree already matches it, would make most integrations fast-forwards. That touches core decision 1 from step 3, so it's waiting for your review.
4. **Every switch appends an apply note**, even when nothing changed, as a ledger of switches. It could be skipped when the applied pointer doesn't move.
5. Design open question 1 (one repo per machine, or shared) is still open. Editor branch names use the hostname, unless `confit.host` is set.
