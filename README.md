# repo-template

Starter template for new zackwag repos. Handles the things a GitHub
"template repository" can't carry over on its own: branch protection,
squash-merge-only, and Conventional Commits enforcement.

## Usage

1. Create the new repo from this template:
   ```
   gh repo create OWNER/NEW-REPO --template zackwag/repo-template --public
   ```
2. Clone it, then run the setup script once from inside it:
   ```
   ./scripts/configure-repo.sh
   ```
   This applies to the new repo's default branch:
   - Squash merge only (merge commits and rebase merging disabled), with
     the PR title always used as the squash commit message
   - "Conventional Commits" required as a status check (see below)
   - Branch protection: no force pushes, no deletions, enforced for admins too
   - No required PR review (matches the account-wide default)

   If the repo has other CI, pass the job name(s) to also require as status
   checks — for example, the two lint jobs described below:
   ```
   ./scripts/configure-repo.sh OWNER/NEW-REPO "ShellCheck" "actionlint"
   ```

3. Give release-please a personal access token (fine-grained, with
   read/write access to the repo's contents and pull requests):
   ```
   gh secret set RELEASE_PLEASE_TOKEN -R OWNER/NEW-REPO
   ```
   The setup script warns if it's missing. See [Releases](#releases) for
   why `GITHUB_TOKEN` won't do.

4. Delete or replace `.github/workflows/test.yml` — it's just a placeholder
   showing the "Test" job name convention used elsewhere in the account.

## Conventional Commits

`.github/workflows/conventional-commits.yml` enforces
[Conventional Commits](https://www.conventionalcommits.org) formatting
(`feat:`, `fix:`, `chore:`, etc.) on whatever actually lands on the default
branch:

- On a PR, it lints the **PR title** — since squash merges use the PR title
  as the commit message, that's what ends up in history.
- On a direct push to `main`/`master` (no PR), it lints the **commit
  message** instead, so pushing straight to the default branch still works
  as long as the message follows the format.

Both paths report under the same "Conventional Commits" check name, so
either one satisfies the required status check set by the setup script.

## Linting

`.github/workflows/lint.yml` lints the template's own content on every push
and PR to `main`:

- **ShellCheck** on everything under `scripts/`
- **actionlint** on the workflow files under `.github/workflows/`

Both are separate jobs so either can be required as its own status check
(see the setup script usage above). Neither is required by default —
`configure-repo.sh` only requires "Conventional Commits" unless you pass
additional job names.

## Releases

`.github/workflows/release-please.yml` runs
[release-please](https://github.com/googleapis/release-please) on every push
to `main`: it keeps a release PR up to date from Conventional Commits, and on
merge tags a GitHub Release and updates `CHANGELOG.md`.

- `release-please-config.json` defaults to `release-type: simple` (no
  package manifest assumed) with `include-component-in-tag: false`, so tags
  are plain `vX.Y.Z` rather than `<component>-vX.Y.Z`. Change `release-type`
  (e.g. to `node`) if the repo has a package manifest release-please should
  bump.
- `.release-please-manifest.json` starts at `0.0.0` and `initial-version` is
  `0.1.0`, so a new repo's first release is `v0.1.0` with an empty
  changelog before it. The template keeps them that way by not releasing
  itself: the job is skipped when the repo is a template.
- It runs with the `RELEASE_PLEASE_TOKEN` secret, not `GITHUB_TOKEN`.
  GitHub doesn't start other workflows for events `GITHUB_TOKEN` creates, so
  with it a workflow on `release` or a tag push (like a Docker Hub publish)
  never runs, and the release PR's checks never report, which blocks merging
  it while "Conventional Commits" is required.
- For an npm package, add a `publish-npm` job gated on this workflow's
  `release_created` output (see e.g. homebridge-somneo's workflow for the
  pattern) to publish on release. `tag_name` is exposed too.

## Dependabot

`.github/dependabot.yml` checks the workflows' actions weekly, so a new
repo doesn't start out pinned to whatever versions the template had. Add
an entry for each package ecosystem the repo uses (`gomod`, `pip`, `npm`,
`docker`, ...).

## Note on private repos

Branch protection is a GitHub Pro feature for private repositories on
personal accounts (this account is on Free). For a private repo, the script
will still apply squash-merge-only but skip branch protection with a clear
message. Public repos aren't affected by this limit.

## Publishing a Docker image

For a repo that ships a container, copy
[phila-trashday's `release.yml`](https://github.com/zackwag/phila-trashday/blob/main/.github/workflows/release.yml)
and change `IMAGE_NAME`. On each published release it builds
`linux/amd64` and `linux/arm64` images and pushes them to Docker Hub tagged
with the version, `major.minor` and `latest`. It needs the
`DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` secrets, and it only runs if
release-please publishes the release with `RELEASE_PLEASE_TOKEN` (see
[Releases](#releases)). If a run fails, or a release went out without
triggering it, rebuild that release by hand:

```sh
gh workflow run release.yml -f tag=v0.1.0
```
