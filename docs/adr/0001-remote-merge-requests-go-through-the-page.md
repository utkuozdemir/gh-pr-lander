# Remote merge requests go through the page, not a mark on the pull request

The owner wants to ask for a merge from a phone or a browser while the terminal app is not running. A mark on the pull request (a comment, a reaction or a label) that an always-on server picks up while polling would be far less code and would work in every GitHub client. We serve our own page from the server instead, because a mark is visible to everyone on the pull request and the owner wants the request to leave no trace there.

## Consequences

The server needs a web view, a way to be reached from the owner's devices and its own protection, none of which a mark on GitHub would need.
