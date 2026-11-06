package core

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/UnstoppableMango/confit/internal/gitrepo"
)

// Sync (docs/multi-host.md, decisions 8 and 9). Every host shares one remote
// repository. A host fetches before it captures or applies and pushes after
// it writes. There is no daemon. Editor branches, applied pointers and
// notes refs belong to one host each: the owner force-pushes its own and
// mirrors everyone else's. Only the integration branch is shared, so it is
// the only ref that is ever merged or retried.
//
// Not reaching the remote is never an error. The work is done locally and
// pushed by the next command that gets through.

// SyncResult is what one pull or push did.
type SyncResult struct {
	Remote string `json:"remote,omitempty"`
	// Integration is what happened to the local integration branch:
	// UpToDate, FastForward, Merged or Conflict; empty if nothing was fetched.
	Integration string   `json:"integration,omitempty"`
	Conflicts   []string `json:"conflicts,omitempty"`
	Pushed      bool     `json:"pushed,omitempty"`
	// Warnings are failures that didn't stop the command, such as being offline.
	Warnings []string `json:"warnings,omitempty"`
}

// pushAttempts is how often a push rejected by another host's push of the
// integration branch is retried after pulling again.
const pushAttempts = 3

// remoteNS is where fetched refs land before they are merged or mirrored.
func (c Config) remoteNS() string { return "refs/confit/remotes/" + c.Remote + "/" }

// syncEnabled reports whether the configured remote exists.
func (b *Buffer) syncEnabled() bool {
	return b.Repo.ConfigValue("remote."+b.Config.Remote+".url") != ""
}

// Pull fetches from the remote, merges its integration branch into the
// local one, mirrors other hosts' refs, and reloads the manifest.
func (b *Buffer) Pull() (SyncResult, error) {
	res := SyncResult{}
	if !b.syncEnabled() {
		return res, nil
	}
	res.Remote = b.Config.Remote
	unlock, err := b.Repo.Lock()
	if err != nil {
		return res, err
	}
	defer unlock()
	if _, err := b.pull(&res); err != nil {
		return res, err
	}
	return res, b.reload()
}

// Push sends the integration branch and this host's own refs to the remote.
// When another host pushed the integration branch first, it pulls, merges
// and tries again.
func (b *Buffer) Push() (SyncResult, error) {
	res := SyncResult{}
	if !b.syncEnabled() {
		return res, nil
	}
	res.Remote = b.Config.Remote
	unlock, err := b.Repo.Lock()
	if err != nil {
		return res, err
	}
	defer unlock()
	defer func() {
		if err := b.reload(); err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
	}()
	for attempt := 0; ; attempt++ {
		specs, err := b.pushSpecs(res.Integration != Conflict)
		if err != nil {
			return res, err
		}
		rejected, err := b.push(specs)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("could not push to %s: %v", b.Config.Remote, err))
			return res, nil
		}
		if !rejected {
			res.Pushed = true
			return res, nil
		}
		if attempt+1 == pushAttempts {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s on %s kept changing; pushed this host's refs only",
				b.Config.Integration, b.Config.Remote))
			return res, nil
		}
		if fetched, err := b.pull(&res); err != nil || !fetched {
			return res, err
		}
	}
}

// pull does Pull's work; the caller holds the lock. It reports whether the
// remote could be reached.
func (b *Buffer) pull(res *SyncResult) (bool, error) {
	ns := b.Config.remoteNS()
	if _, err := b.Repo.Git("fetch", "--quiet", "--no-tags", "--prune", "--no-write-fetch-head", b.Config.Remote,
		"+refs/heads/*:"+ns+"heads/*", "+"+notesPrefix+"*:"+ns+"notes/*"); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("could not fetch from %s: %v", b.Config.Remote, err))
		return false, nil
	}
	if err := b.mergeRemote(res); err != nil {
		return true, err
	}
	return true, b.mirror()
}

// mergeRemote brings the fetched integration branch into the local one: a
// fast-forward when possible, else a merge. A conflicting merge leaves the
// local branch alone until a person merges by hand.
func (b *Buffer) mergeRemote(res *SyncResult) error {
	ref := b.Config.integrationRef()
	theirsRef := b.Config.remoteNS() + "heads/" + b.Config.Integration
	for attempt := 0; attempt < 5; attempt++ {
		res.Conflicts = nil
		theirs, err := b.Repo.Resolve(theirsRef)
		if err != nil {
			return err
		}
		ours, err := b.Repo.Resolve(ref)
		if err != nil {
			return err
		}
		if theirs.IsZero() {
			res.Integration = UpToDate
			return nil
		}
		next := theirs
		res.Integration = FastForward
		if !ours.IsZero() {
			if in, err := b.Repo.IsAncestor(theirs, ours); err != nil || in {
				res.Integration = UpToDate
				return err
			}
		}
		ff, err := b.ffOrZero(ours, theirs)
		if err != nil {
			return err
		}
		if !ff {
			if ff, err = b.unrelated(ours, theirs, res); err != nil || res.Integration == Conflict {
				return err
			}
		}
		if !ff {
			m, err := b.Repo.MergeTree(ours, theirs, ref)
			if err != nil {
				return err
			}
			if !m.Clean {
				res.Integration, res.Conflicts = Conflict, m.Conflicts
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"%s conflicts with %s/%s in %s; merge it by hand", b.Config.Integration, b.Config.Remote,
					b.Config.Integration, strings.Join(m.Conflicts, ", ")))
				return nil
			}
			msg := fmt.Sprintf("Merge %s/%s into %s\n\n", b.Config.Remote, b.Config.Integration, b.Config.Integration) +
				trailers("Confit-Synced", b.Config.Remote)
			if next, err = b.Repo.WriteCommit(gitrepo.Commit{Tree: m.Tree, Parents: []gitrepo.Hash{ours, theirs},
				Author: b.human(), Committer: b.committer("sync"), Message: msg}); err != nil {
				return err
			}
			res.Integration = Merged
		}
		err = b.Repo.UpdateRefs("confit sync", gitrepo.RefUpdate{Name: ref, New: next, Old: ours})
		if errors.Is(err, gitrepo.ErrRefChanged) {
			continue
		}
		return err
	}
	return fmt.Errorf("%s kept changing while syncing", b.Config.Integration)
}

// unrelated handles an integration branch with no history in common with
// the remote's, as on a host that ran confit init before adding the remote.
// A branch holding nothing but confit's initial commit is replaced; anything
// else is left for a person. It reports whether to take theirs as is.
func (b *Buffer) unrelated(ours, theirs gitrepo.Hash, res *SyncResult) (bool, error) {
	if _, err := b.Repo.Git("merge-base", ours.String(), theirs.String()); err == nil {
		return false, nil
	}
	c, err := b.Repo.ReadCommit(ours)
	if err != nil {
		return false, err
	}
	if len(c.Parents) == 0 && strings.HasPrefix(c.Message, "confit: initialize\n") {
		return true, nil
	}
	res.Integration = Conflict
	res.Warnings = append(res.Warnings, fmt.Sprintf("%s and %s/%s have no history in common; merge them by hand",
		b.Config.Integration, b.Config.Remote, b.Config.Integration))
	return false, nil
}

func (b *Buffer) ffOrZero(ours, theirs gitrepo.Hash) (bool, error) {
	if ours.IsZero() {
		return true, nil
	}
	return b.Repo.IsAncestor(ours, theirs)
}

// mirror copies other hosts' editor branches, applied pointers and notes
// refs from the fetched ones. This host's own are never overwritten.
func (b *Buffer) mirror() error {
	ns := b.Config.remoteNS()
	var ups []gitrepo.RefUpdate
	for _, area := range []struct{ fetched, local string }{
		{ns + "heads/" + b.Config.EditsPrefix, "refs/heads/" + b.Config.EditsPrefix},
		{ns + "heads/" + b.Config.AppliedPrefix, "refs/heads/" + b.Config.AppliedPrefix},
		{ns + "notes/", notesPrefix},
	} {
		fetched, err := b.Repo.ListRefs(area.fetched)
		if err != nil {
			return err
		}
		for name, h := range fetched {
			local := area.local + strings.TrimPrefix(name, area.fetched)
			if b.owns(local) {
				continue
			}
			old, err := b.Repo.Resolve(local)
			if err != nil {
				return err
			}
			if old != h {
				ups = append(ups, gitrepo.RefUpdate{Name: local, New: h, Old: old})
			}
		}
	}
	if len(ups) == 0 {
		return nil
	}
	// Another host's refs change only by sync, so a lost race here is
	// repaired by the next pull.
	if err := b.Repo.UpdateRefs("confit sync: mirror", ups...); err != nil && !errors.Is(err, gitrepo.ErrRefChanged) {
		return err
	}
	return nil
}

// owns reports whether a local ref is this host's to write: its editor
// branches (<adapter>@<host>), its notes ref, and the applied pointers of
// its consumers, which are the ones in the manifest or named <x>@<host>.
func (b *Buffer) owns(ref string) bool {
	c := b.Config
	suffix := "@" + c.Host
	if name, ok := strings.CutPrefix(ref, "refs/heads/"+c.EditsPrefix); ok {
		return strings.HasSuffix(name, suffix)
	}
	if name, ok := strings.CutPrefix(ref, "refs/heads/"+c.AppliedPrefix); ok {
		_, configured := c.Consumers[name]
		return configured || strings.HasSuffix(name, suffix)
	}
	return ref == c.notesRef()
}

// pushSpecs lists what this host pushes: the integration branch (never
// forced, and left out while it conflicts with the remote) and its own refs
// (forced, since no other host writes them).
func (b *Buffer) pushSpecs(integration bool) ([]string, error) {
	var specs []string
	if integration {
		specs = append(specs, b.Config.integrationRef()+":"+b.Config.integrationRef())
	}
	var own []string
	for _, prefix := range []string{"refs/heads/" + b.Config.EditsPrefix, "refs/heads/" + b.Config.AppliedPrefix, b.Config.notesRef()} {
		refs, err := b.Repo.ListRefs(prefix)
		if err != nil {
			return nil, err
		}
		for name := range refs {
			if b.owns(name) {
				own = append(own, "+"+name+":"+name)
			}
		}
	}
	sort.Strings(own)
	return append(specs, own...), nil
}

// push runs git push. It reports whether the remote rejected a ref because
// it moved (another host pushed first); any other failure is an error.
func (b *Buffer) push(specs []string) (rejected bool, err error) {
	if len(specs) == 0 {
		return false, nil
	}
	out, err := b.Repo.Git(append([]string{"push", "--porcelain", "--quiet", b.Config.Remote}, specs...)...)
	if err == nil {
		return false, nil
	}
	// Porcelain lines: <flag> TAB <src>:<dst> TAB <summary>; "!" is rejected.
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(l, "\t")
		if len(f) >= 3 && f[0] == "!" && (strings.Contains(f[2], "fetch first") || strings.Contains(f[2], "non-fast-forward")) {
			rejected = true
		}
	}
	if rejected {
		return true, nil
	}
	return false, err
}

// reload re-reads the manifest from the integration branch, which a pull
// may have moved.
func (b *Buffer) reload() error {
	tip, err := b.integrationTip()
	if err != nil {
		return err
	}
	m, err := b.readManifest(tip)
	if err != nil {
		return err
	}
	return b.loadManifest(m)
}
