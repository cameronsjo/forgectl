package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/surface/merge"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
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
// A launch by hand (drain false) reads it best-effort: a failed read prints
// one warning line to warn, records no identity (so the row can never pass
// the merge policy), and the launch goes on. A drain launch fails on it,
// wrapping errIdentityRead, so the drain pauses claiming rather than spend
// the row's attempts on a GitHub outage.
//
// A drain launch (drain true) also refuses a branch that already exists: in
// the checkout, as origin/<branch>, or on GitHub. Its error wraps
// worker.ErrBranchExists and names the way out, and nothing has been
// created when it returns.
func workerRepoIdentity(ctx context.Context, run exec.Runner, warn io.Writer, top, branch string, drain bool) (repoIdentity, error) {
	if drain {
		if err := worker.CheckBranchNew(ctx, run, top, branch); err != nil {
			return repoIdentity{}, err
		}
	}
	owner, name, ok := githubOriginOf(ctx, run, top)
	if !ok {
		return repoIdentity{}, nil
	}
	id, err := readRepoIdentity(ctx, run, owner, name, branch)
	if err != nil {
		if !drain {
			_, _ = fmt.Fprintf(warn, "forgectl: warning: %s; this worker records no GitHub repository, so the merge policy never passes it\n",
				termsafe.SafeLineMax(err.Error(), maxIdentityWarning))
			return repoIdentity{}, nil
		}
		return repoIdentity{}, err
	}
	if drain && id.RefExists {
		return repoIdentity{}, fmt.Errorf("%w: %s exists on GitHub (%s); a drain worker starts on a new branch: %s",
			worker.ErrBranchExists, branch, id.NameWithOwner, worker.BranchExistsRemedy(top, branch))
	}
	return repoIdentity{NameWithOwner: id.NameWithOwner, DatabaseID: id.DatabaseID}, nil
}

// maxIdentityWarning caps the identity-read error a hand launch prints.
const maxIdentityWarning = 300

// errIdentityRead marks a failed read of the repository identity: gh failed
// or GitHub's answer was unusable. The drain pauses claiming on it
// (drain.ErrGitHubRead) instead of counting an attempt.
var errIdentityRead = errors.New("the repository identity could not be read from GitHub")

// errIdentityRefused marks an identity read GitHub answered but refused or
// answered without a repository: a renamed, deleted or inaccessible
// repository, or a GraphQL error. Retrying on the next tick would only find
// it again, so the drain counts an attempt rather than pausing; a row stuck
// on it fails at the attempt limit instead of holding the queue.
var errIdentityRefused = errors.New("GitHub refused the repository identity read")

// transientGitHubFailure reports a gh failure worth pausing for rather than
// counting: the network, a server error, or a rate limit. Anything else is
// treated as GitHub's answer.
func transientGitHubFailure(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"http 5", "rate limit", "dial tcp", "connection refused", "connection reset",
		"no such host", "i/o timeout", "tls handshake", "network is unreachable", "context deadline exceeded",
		"timeout awaiting", "unexpected eof"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// readRepoIdentity runs IdentityQuery for owner/name and branch.
func readRepoIdentity(ctx context.Context, run exec.Runner, owner, name, branch string) (merge.Identity, error) {
	slug := owner + "/" + name
	gh := githubauth.Runner(run, githubauth.DefaultHost)
	out, err := gh.Run(ctx, "gh", "api", "graphql", "--hostname", githubauth.DefaultHost,
		"-f", "query="+merge.IdentityQuery,
		"-f", "owner="+owner, "-f", "name="+name, "-f", "ref=refs/heads/"+branch)
	if err != nil {
		class := errIdentityRefused
		if transientGitHubFailure(err) {
			class = errIdentityRead
		}
		return merge.Identity{}, fmt.Errorf("forgectl: read %s's repository name and id from GitHub, which a worker launch records: %w: %w", slug, class, err)
	}
	id, err := merge.DecodeIdentity([]byte(out))
	if err != nil {
		return merge.Identity{}, fmt.Errorf("forgectl: read %s's repository name and id from GitHub, which a worker launch records: %w: %w", slug, errIdentityRefused, err)
	}
	return id, nil
}

// errNoIdentityStep refuses a worker launch wired without its identity
// step, so a launch path that forgot it fails rather than recording a row
// the merge policy cannot tie to a repository.
var errNoIdentityStep = errors.New("forgectl: the worker launch has no repository identity step")
