# Multi-host design

Decided with Erik on 2026-10-17, after step 4 ([end-to-end.md](end-to-end.md)). It settles that page's open decisions and the design's open question 1 (one repo per machine or shared). Stage 1 of the build plan at the end is built.

## Decisions

| # | Decision |
|---|---|
| 1 | home-manager applies by running `confit apply` during activation, not by reading the repo as a flake input. |
| 2 | A key in both `dconf.settings` and the manifest: Nix wins on every switch, and the UI value is still recorded. |
| 3 | A new edit starts from `desired` when the editor's layer already matches it, so integration fast-forwards: one commit per capture. |
| 4 | No apply note when a switch doesn't move that host's applied pointer. |
| 5 | **One repo shared by every machine.** Branches are the muxing point. |
| 6 | One shared `desired`. A host can still set its own integration branch. |
| 7 | Consumers are named per host by default: `home-manager@<host>`. |
| 8 | Sync without a daemon: fetch before capture and apply, push after integrate, and on a rejected push fetch, integrate again and retry. |
| 9 | Offline, capture and apply work on the local repo, and the push happens on the next run that reaches the remote. |
| 10, 11 | What each host tracks, and where its edits go, lives in a manifest committed on `desired`. Git config keeps only what is local to a machine. |
| 12 | Two layers: shared, and per host under `hosts/<host>/`. The host layer wins on apply. Edits go to the shared layer unless the manifest says otherwise. |
| 13 | Tracking is an allowlist of prefixes. Anything unlisted is never captured or applied. |
| 14 | The home-manager module shrinks to package, repo, remote and host. One `confit watch` per host reads the manifest, replacing the per-file `.path` units. |
| 15 | Layers combine per key where confit understands the format (dconf keys, top-level JSON keys), and per file otherwise. |
| 16 | A host missing from the manifest adds itself as a commit, tracking only the shared layer. |
| 17 | No migration from the git-config setup. |

## Repo layout

```
confit.toml                      # the manifest
dconf/org/gnome/desktop/...      # shared layer, layout unchanged
vscode/settings.json
hosts/laptop/dconf/...           # laptop's layer: only the keys it overrides
hosts/laptop/vscode/settings.json
```

## Manifest

```toml
[adapters.dconf]
type = "dconf"

[adapters.vscode]
type = "file"
files = { "settings.json" = "~/.config/Code/User/settings.json" }

[hosts.laptop]
track.dconf  = ["/org/gnome/desktop/interface/", "/org/gnome/desktop/wm/"]
track.vscode = ["settings.json"]
local.dconf  = ["/org/gnome/desktop/interface/text-scaling-factor"]
local.vscode = ["settings.json:window.zoomLevel"]

[hosts.laptop.consumers."home-manager@laptop"]
adapters = ["dconf", "vscode"]
drift = "adopt"
```

- **`track`** is the allowlist (13). A dconf entry is a key or a path prefix. A file entry is a file name, optionally `file:key` for a top-level JSON key.
- **`local`** lists what goes to the host's layer (12). Everything else that is tracked goes to the shared layer, except a key the host layer already holds, which stays there (writing it to the shared layer would be undone by the host's next apply). Anything local is tracked.
- **`"*"`** tracks everything. `confit adapter add` uses it unless given `--track`.
- **Live paths** live on the adapter, with `~` expanded per machine. A host can override them if it needs to.
- **Consumers** are per host, under `[hosts.<host>.consumers.<name>]`, with `adapters`, `drift` and `source`. Stage 2 defaults the name to `home-manager@<host>` (7).
- **Git config** keeps `confit.host` (default: the hostname) and `confit.remote` (default `origin`), plus the merge-driver entries, which can't travel with the repo anyway. The integration branch name and the `edits/` and `applied/` prefixes stay there too: the manifest lives on the integration branch, so it can't name it.
- **A new host** (16) commits `[hosts.<host>]` with `track = "shared"` on its first capture, prepare or apply. That means every key and file already in the shared layer (as `desired` has it), and nothing local.
- **Manifest edits** are ordinary commits to `desired`. A host reads the manifest from the `desired` it is about to apply, so a change to tracking takes effect at the next apply.

## Capture and apply with layers

- **Capture** reads the live state of every tracked prefix. Each changed key goes to the host layer if a `local` entry covers it, otherwise to the shared layer. Untracked keys are never read into the repo.
- **Apply** writes the shared layer overlaid with the host layer, key by key (15). For opaque files the host's copy replaces the shared one. Keys outside the host's `track` are left alone on the live system.
- **Drift** works as today, on the effective (overlaid) state.

## History (3, 4)

The editor branch for a capture is `edits/<adapter>@<host>`, as today. When the editor's tree already matches `desired` for the layers it touches, the new commit is made on top of `desired`, so integrating it is a fast-forward. Otherwise it is made on the editor branch and merged, as today. The apply note is skipped when the applied pointer would not move.

## Sync (8, 9)

1. **Fetch** `desired`, `edits/*`, `applied/*` and `refs/notes/confit-applied/*` from `confit.remote` before every capture and apply. Failure to reach the remote is a warning, not an error.
2. **Capture and integrate** locally, as today.
3. **Push** `desired`, this host's editor branches, its applied pointer and its notes ref. On a rejected `desired`, fetch, integrate again, and retry a few times. A real conflict is recorded the way it is today and isn't pushed.
4. **Apply notes** move to a notes ref per host, `refs/notes/confit-applied/<host>`. Each host writes only its own, so notes never need merging. A line-based merge such as `cat_sort_uniq` would split the multi-line apply records.

Applied pointers, editor branches and notes refs are all per host, so only `desired` sees contention.

## home-manager module (1, 2, 14)

- **Options:** `enable`, `package`, `repo`, `remote` (a URL, cloned on first switch), `host` (default: the hostname), `apply`.
- **Activation:** capture before `writeBoundary` and apply after `dconfSettings`, as today. A key also set in `dconf.settings` is overwritten by the switch, and the UI value is still recorded (2).
- **Watching:** one `confit watch --all` user service follows `dconf watch` and inotify for whatever the manifest tracks on this host, and reloads when the manifest changes. The hourly capture timer stays.

## Build plan

Each stage is one PR, with review between stages:

1. **Manifest and layers**, on one host. Add the `confit.toml` loader, replacing adapter and consumer git config. Add the allowlist, layer routing, per-key overlay for dconf and top-level JSON, and host self-registration. Core tests cover each.
2. **History.** Skip no-op apply notes, and name consumers per host. (Starting edits from `desired` landed in stage 1: without it, the manifest commits on `desired` turned every capture into a merge.)
3. **Sync.** Add fetch and push with retry, offline behaviour and per-host notes refs. Tests use two clones sharing one bare remote.
4. **Module and end to end.** Slim the module and add the manifest-reading `confit watch --all`. Extend the home-manager e2e to two hosts (two HOMEs) sharing a bare remote, covering a shared change reaching both hosts and a local change staying put.
