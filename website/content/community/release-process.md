---
title: Release Process
weight: 2
---

This is the process to follow to make a new release. This should
really be automated someday, but releases are juuuust infrequent
enough that it's less annoying to just do the work each time.

### Check the issue tracker milestone

Are you actually ready to release? Check the milestone on github and
verify that all its issues are closed. If there are open issues,
you'll have to either resolve them, or bump to the next version.

### Check for outdated dependencies

This might be a little late to do at release time, but releases are not so
frequent for now, so it makes sense to do this before creating a release.

Some dependencies are usually best to stay fresh, like client-go and friends.
But ideally we should be using recent versions of all our dependencies.

If client-go is too old (like 2-3 releases old), from time to time Kubernetes
may remove some field that breaks MetalLB if it is using an old version. Make
sure either to use a recent version or create an issue to do a new release,
after proper testing, to update client-go.

### Finalize release notes

All release notes are always written on the `main` branch, and
copied into release branches in a later step. Point out all new
features and actions required by users. If there are very notable
bugfixes (e.g. security issues, long-term pain point resolved), point
those out as well.

Also update the documentation link so that the soon-to-be latest
release's documentation link points to `metallb.universe.tf`, and the
previous releases point to `vX-Y-Z--metallb.netlify.com`, which is the
website pinned at that tagged release.

To get a list of contributors to the release, run `git log
--format="%aN" $(git merge-base CUR-BRANCH PREV-TAG)..CUR-BRANCH | sort -u |
tr '\n' ',' | sed -e 's/,/, /g'`. `CUR-BRANCH` is `main` if you're
making a minor release (e.g. 0.9.0), or the release branch for the
current version if you're making a patch release (e.g. `v0.8` if
you're making `v0.8.4`). `PREV-TAG` is the release tag name for the
last release (e.g. if you're preparing 0.8.4, `PREV-TAG` is
`v0.8.3`. Also think about whether there were significant
contributions that weren't in the form of a commit, and include those
people as well. It's better to err on the side of being _too_
thankful!

Commit the finalized release notes.

### Clean the working directory

The release script only works if the Git working directory is
_completely_ clean: no pending modifications, no untracked files,
nothing. Make sure everything is clean, or run the release from a
fresh checkout.

The release script will abort if the working directory isn't right.

### Run the release script 

Run `inv release X.Y.Z`. This will create the appropriate branches,
commits and tags in your local repository.

### Push the new artifacts

Run `git push --tags origin main vX.Y`. This will push all pending
changes both in main and the release branch, as well as the new tag
for the release.

### Protect the release branch (skip for patch releases)

For major and minor releases, the release script created a new `vX.Y`
branch. Go into github's repository settings and mark the branch
protected, including from administrators, to guard against accidental
force pushes.

### Create a new release on github

By default, new tags show up de-emphasized in the list of
releases. Create a new release attached to the tag you just
pushed. Make the description point to the release notes on the
website.

### Wait for the image repositories to update

When you pushed, CircleCI kicked off a set of image builds for the new
tag. You need to wait for these images to be pushed live before
continuing, because the manifests for the new release point to image
tags that don't exist until CircleCI makes them exist.

Check on Quay for a `vX.Y.Z` tag on each image, or check on
CircleCI that the deploy has completed.

### Repoint the live website

Move the `live-website` branch to the newly created tag with `git
branch -f live-website vX.Y.Z`, then force-push the branch with `git
push -f origin live-website`. This will trigger Netlify to
redeploy [metallb.universe.tf](https://metallb.universe.tf) with
updated documentation for the new version.

### Brag about new release

Tweet, post to G+, slack, IRC, whatever. Make some noise, if it's
worth making noise about!
