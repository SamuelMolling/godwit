# 0028 — A branch names which bound target, and the binding still permits it

## The question

A repository whose default branch is staging applies there, and the pull request that merges staging into main applies the same files to production. One `target` key cannot name both databases, and the person commenting should not have to name an environment.

## The decision

`godwit.yaml` may carry `targets`, a map from destination branch to target name. A pull request and a comment use the pull request's base. A push uses the branch it updates. `bindings.grant` is unchanged: a name in the file that this repository is not bound to is refused, and the refusal does not list the names it does hold.

A file with only `target` is unchanged. Pull requests into any base use that target, and only a push to the default branch verifies it, so a repository that has not opted in does not start verifying every branch. A push to a branch the map does not name is ignored. A pull request into such a branch is refused on that pull request.

## What was refused

Storing the branch on the server binding. The map would then be invisible on the pull request the reviewer is reading.
