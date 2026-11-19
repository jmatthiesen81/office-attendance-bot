# ADR 0004: Trunk branch `canon` instead of version branches

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

Until now, development happened on a version branch named after the release line (`1.0`). It was the default branch on GitHub, and every push to it published the `edge` image. Each new feature release (1.1, 1.2, 1.3) would have needed a new branch `1.1`, `1.2`, … and a switch of the default branch. That never happened, so `1.0` has carried every release since 1.1.0 and its name is misleading.

The version is already fully defined by the Git tag: `1.3.1` publishes the image tags `1.3.1`, `1.3`, `1` and `latest`. Version branches only pay off if several release lines are maintained in parallel, e.g. fixes for 1.2.x while 1.3 is current. That is not the case here: there is one production instance, and it follows `:1`.

## Decision

The project works trunk-based with a single long-lived branch named **`canon`**.

- **`canon`** is the default branch. Every push publishes `:edge` and `:sha-<commit>` for live testing.
- **Feature branches** (`feat/<topic>`, `fix/<topic>`, …) branch off `canon` and are merged back into it.
- **Releases** are only Git tags on `canon` (`1.4.0`, `1.4.1`, …). No branch is created for a release.
- **Old release lines** are only maintained when actually needed. The branch is then created from the tag: `git switch -c 1.3 1.3.1`, commit the fix, tag `1.3.2`. Such branches do not publish `:edge`.

### Why `canon`

The name is deliberately individual instead of `main`, `master` or `trunk`. The metaphor matches the workflow: feature branches are fan fiction, ideas that are not official yet; merging makes them canon; a release tag is a published edition of the canon. It also echoes "canonical", the authoritative source, so the name is understandable without knowing the joke.

Rejected: `origin` and `HEAD` (already taken by Git), names that look like versions (they would collide with tag patterns), `edge` (would mix up branch and image tag).

## Consequences

- No more switching the default branch for a feature release.
- `release.yml` triggers on `canon` instead of the pattern `[0-9]+.[0-9]+`.
- Dependabot has no `target-branch` and automatically uses the default branch.
- Anyone with an existing clone has to rename the local branch once (see below).

## Migration (reusable for other projects)

1. **Prepare the repository** in a feature branch and merge it into the old default branch:
   - CI triggers: replace branch patterns with `canon` (GitHub Actions: `on.push.branches`; GitLab CI: `rules`/`only` with `$CI_DEFAULT_BRANCH` or `canon`).
   - Search for the old branch name: `git grep -nwE "<old>|main|master"` in workflows, `dependabot.yml` (`target-branch`), Renovate config, Makefile, scripts, badges and docs.
2. **Rename the branch on the server:**
   - **GitHub:** *Settings → General → Default branch*, pencil icon next to the branch, rename to `canon`. GitHub retargets open pull requests, updates branch protection rules that name the branch explicitly and redirects old links. Rulesets with patterns must be checked by hand.
   - **GitLab** (no rename): `git push origin <old>:refs/heads/canon`, then *Settings → Repository → Branch defaults* set `canon` as default, move protected-branch settings to `canon`, retarget open merge requests, then delete the old branch.
3. **Update each local clone:**
   ```sh
   git branch -m <old> canon
   git fetch origin --prune
   git branch -u origin/canon canon
   git remote set-head origin -a
   ```
4. **Verify:** push a commit to `canon` and check that CI publishes `:edge`; the next release is a tag on `canon`.
