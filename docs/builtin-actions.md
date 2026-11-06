# Built-in actions

Built-in actions ship with the runner, so a job needs no action download and no Node in its image.

```yaml
- uses: builtin:checkout
- uses: builtin:upload-artifact
- uses: builtin:download-artifact
```

## checkout

Works like `actions/checkout` with these inputs. Any other input fails the step. It runs `git` where the job runs, see the [prerequisites](../README.md#prerequisites).

| Input | Default |
| --- | --- |
| `repository` | `github.repository` |
| `ref` | the triggering ref and commit, or the default branch of another repository |
| `token` | `github.token` |
| `ssh-key` | empty, a private key fetches over SSH instead |
| `ssh-known-hosts` | empty, host keys trusted besides `~/.ssh/known_hosts` |
| `ssh-strict` | `true`, rejects unknown host keys |
| `ssh-user` | the user of the repository's SSH URL, else `git` |
| `persist-credentials` | `true`, later steps of the job can fetch and push with `token` or `ssh-key` |
| `path` | workspace root |
| `clean` | `true`, removes untracked files, also in submodules |
| `filter` | empty, a partial clone filter such as `blob:none` |
| `sparse-checkout` | empty, the paths to check out, one per line |
| `sparse-checkout-cone-mode` | `true`, `false` reads `sparse-checkout` as patterns |
| `fetch-depth` | `1`, `0` for the full history of every branch and tag |
| `fetch-tags` | `false`, `true` fetches every tag |
| `show-progress` | `true`, shows the fetch progress |
| `lfs` | `false`, `true` downloads Git LFS files |
| `submodules` | `false`, `true` checks out submodules, `recursive` also theirs |
| `set-safe-directory` | `true`, lets checkout's `git` work in a directory another user owns |

A branch is checked out as a local branch tracking `origin`, a tag or commit as a detached `HEAD`. A triggering tag that moved since the event fails the step. The step outputs `ref` and `commit` like `actions/checkout`.

Persisted credentials end with the job. `token` is only sent to `github.server_url`, while `ssh-key` is offered to any SSH host, as with `actions/checkout`. The runner's `client_cert_file` is only offered to `github.server_url` and only during the step, so later steps do not get it. Its `ca_file` replaces the job image's CA bundle for `github.server_url`, in later steps too when credentials persist. Without `ssh-key`, submodule URLs of the form `git@<server host>:` are fetched from `github.server_url` with `token`. With `ssh-key`, the repository is fetched from the SSH URL in the event's `repository.ssh_url`, or from the server's host on port 22.

With `gitea-runner exec`, checking out the workflow's own repository copies your local working directory, unless `--no-skip-checkout` or `ssh-key` is set.

## upload-artifact

Works like `actions/upload-artifact` v7 with these inputs. Any other input logs a warning, as on GitHub. It needs Gitea 1.22.4 or later, and 1.26 for `archive: false`.

| Input | Default |
| --- | --- |
| `name` | `artifact`, ignored with `archive: false`, which names the artifact after the file |
| `path` | required, files, directories or patterns, one per line, `!` excludes |
| `if-no-files-found` | `warn`, also `error` or `ignore` |
| `retention-days` | the server's default, at most `GITHUB_RETENTION_DAYS` when set |
| `compression-level` | `6`, from `0` for none to `9` |
| `overwrite` | `false`, `true` deletes an artifact of the same name first |
| `include-hidden-files` | `false`, `true` uploads files and directories whose name starts with `.` |
| `archive` | `true`, `false` uploads a single file as is |

Patterns, symlinks and the artifact's root directory follow `actions/upload-artifact`, as do `ACTIONS_ARTIFACT_UPLOAD_CONCURRENCY` and `ACTIONS_ARTIFACT_UPLOAD_TIMEOUT_MS`. The step outputs `artifact-id`, `artifact-digest` and `artifact-url`. On Gitea 1.26 and later, the `artifact-url` is the artifact's link on the run page for this attempt, where `actions/upload-artifact` gives a link only GitHub serves.

## download-artifact

Works like `actions/download-artifact` v8 with these inputs. Any other input logs a warning, as on GitHub.

| Input | Default |
| --- | --- |
| `name` | empty, every artifact of the run |
| `artifact-ids` | empty, comma-separated IDs instead of `name` |
| `path` | workspace root, `~` is the job user's home |
| `pattern` | empty, a glob the artifact names must match, without the extglobs and brace ranges of minimatch |
| `merge-multiple` | `false`, several artifacts go to `path/<name>`, `true` puts them all in `path` |
| `github-token` | empty, a token to download from another run or repository |
| `repository` | `github.repository`, with `github-token` |
| `run-id` | `github.run_id`, with `github-token` |
| `skip-decompress` | `false`, `true` keeps a zipped artifact as `<name>.zip` |
| `digest-mismatch` | `error`, also `ignore`, `info` or `warn` |

Files are written as `actions/download-artifact` writes them, without their mode, so executables lose their executable bit. `digest-mismatch` only applies when the server reports a digest, which Gitea does not. The step outputs `download-path`.

`github-token` needs Gitea 1.24 or later and is only sent to the instance the runner is registered with. Downloads from another run read every page of its artifacts and skip expired ones, where `actions/download-artifact` sees only Gitea's first 30 and fails on an expired newest copy.

With `gitea-runner exec`, both actions use the local artifact server of `--artifact-server-path`, which serves no other runs for `github-token`.
