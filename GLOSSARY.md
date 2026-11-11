# gh-pr-lander

Watching one person's open pull requests and getting them merged with as little attention as possible, in a workflow where every commit is signed and merges are fast-forward only.

## Language

**Fast-forward merge**:
Merging a pull request by moving its base branch to the pull request's head commit, so no new commit is created and signatures stay intact.
_Avoid_: `/m`, merge comment, merge button

**Ready**:
A pull request that is approved, has every required check passing and is up to date with its base branch, so a fast-forward merge would be accepted. A check that is not required may still be running.
_Avoid_: mergeable, green

**Automerge**:
A standing request on one pull request to fast-forward merge it as soon as it becomes ready.
_Avoid_: merge when ready, auto-merge (GitHub's own feature of that name is a different thing)

**Approval**:
A person with write access to the repository whose latest review of the pull request approves it. Each person counts once.
_Avoid_: review, LGTM, approving review

**Unresolved comment**:
A review comment thread that nobody has marked as resolved, whether or not the code under it has changed since.
_Avoid_: open thread, outdated comment

**Automerge condition**:
An extra requirement the owner sets on one pull request's automerge, on top of ready: a minimum number of approvals, or no unresolved comments. It is set before automerge is turned on and stays with the pull request. It holds back automerge only, never a merge the owner starts.
_Avoid_: merge rule, gate

**Terminal app**:
The tool as run on the owner's own machine, where signing is possible. It works on its own, with no server.

**Server**:
An always-on instance of the tool with no terminal, which can do everything that needs no signing.

**Page**:
The server's web view of the owner's pull requests, used from a phone or a browser.

**Remote merge request**:
Asking for automerge on a pull request through the page, from a device that does not run the terminal app. It leaves no trace on the pull request.
