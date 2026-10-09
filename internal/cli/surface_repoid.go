package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/surface/merge"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// A worker launch records which GitHub repository it worked on, as GitHub
// names it (atelier P4, T10.2): the merge policy reads the row's recorded
// nameWithOwner and id, never the checkout's .git/config, which a worker can
// rewrite after it starts. The read runs before the ledger row is written,
// so a failure leaves nothing behind.

// repoIdentity is what a launch records about its repository.
type repoIdentity struct {
	// NameWithOwner and DatabaseID are GitHub's; both are empty when origin
	// is not on github.com.
	NameWithOwner string
	DatabaseID    int64
}

// githubOriginOf reads owner and name from top's origin remote. ok is false
// when origin is missing, unreadable, or on another host (an SSH host alias
// for GitHub counts as another host, as it does for the base lookup).
func githubOriginOf(ctx context.Context, run exec.Runner, top string) (owner, name string, ok bool) {
	origin, err := gitenv.Run(ctx, run, gitenv.Local, "-C", top, "remote", "get-url", "origin")
	if err != nil {
		return "", "", false
	}
	host, owner, name, parsed := pr.ParseRemoteURL(origin)
	if !parsed || (host != githubauth.DefaultHost && host != "ssh.github.com") {
		return "", "", false
	}
	if !pr.ValidOwnerRepoPart(owner) || !pr.ValidOwnerRepoPart(name) {
		return "", "", false
	}
	return owner, name, true
}

// workerRepoIdentity reads the repository identity a launch records, with
// one `gh api graphql` query pinned to github.com. A repository whose origin
// is not on github.com records none, and the launch goes on; its row is
// never eligible for a merge.
//
// A drain launch (drain true) also refuses a branch that already exists: in
// the checkout, as origin/<branch>, or on GitHub. Its error wraps
// worker.ErrBranchExists, and nothing has been created when it returns.
func workerRepoIdentity(ctx context.Context, run exec.Runner, top, branch string, drain bool) (repoIdentity, error) {
	if drain {
		if err := worker.CheckBranchNew(ctx, run, top, branch); err != nil {
			return repoIdentity{}, err
		}
	}
	owner, name, ok := githubOriginOf(ctx, run, top)
	if !ok {
		return repoIdentity{}, nil
	}
	slug := owner + "/" + name
	gh := githubauth.Runner(run, githubauth.DefaultHost)
	out, err := gh.Run(ctx, "gh", "api", "graphql", "--hostname", githubauth.DefaultHost,
		"-f", "query="+merge.IdentityQuery,
		"-f", "owner="+owner, "-f", "name="+name, "-f", "ref=refs/heads/"+branch)
	if err != nil {
		return repoIdentity{}, fmt.Errorf("forgectl: read %s's repository name and id from GitHub, which a worker launch records: %w", slug, err)
	}
	id, err := merge.DecodeIdentity([]byte(out))
	if err != nil {
		return repoIdentity{}, fmt.Errorf("forgectl: read %s's repository name and id from GitHub, which a worker launch records: %w", slug, err)
	}
	if drain && id.RefExists {
		return repoIdentity{}, fmt.Errorf("%w: %s exists on GitHub (%s); a drain worker starts on a new branch", worker.ErrBranchExists, branch, id.NameWithOwner)
	}
	return repoIdentity{NameWithOwner: id.NameWithOwner, DatabaseID: id.DatabaseID}, nil
}

// errNoIdentityStep refuses a worker launch wired without its identity
// step, so a launch path that forgot it fails rather than recording a row
// the merge policy cannot tie to a repository.
var errNoIdentityStep = errors.New("forgectl: the worker launch has no repository identity step")
