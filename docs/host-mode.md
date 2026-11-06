# Host mode

## Stable working directory

Host jobs run in a random `<host.workdir_parent>/<16 hex>/hostexecutor` directory by default. With `host.stable_workdir: true` they run in `<host.workdir_parent>/workspaces/<owner>/<repo>/<slot>/hostexecutor`, where `slot` is a free `runner.capacity` slot starting at `0`.

- The path is fully stable only at `runner.capacity: 1`. At higher capacity it depends on which slot the job gets.
- Only the path is stable. The directory is removed after each job, so no checkout or other state carries over.
- A job uses a random directory when its repository name is unsafe in a path. It does too, with a warning in its log, when the stable directory is taken: by a parallel job of a called workflow, another runner sharing `host.workdir_parent`, or an earlier job's failed removal. Idle cleanup removes such leftovers after `runner.workdir_cleanup_age`.
- Concurrent jobs still share the host, so a stable path gives no isolation.

## Background processes

Host jobs clean up background step processes at job completion. A step can opt out:

```yaml
steps:
  - name: Start shared helper
    env:
      RUNNER_TRACKING_ID: ""
    run: Start-Process vendor-helper.exe
```

- Set the `RUNNER_TRACKING_ID` override in the step's `env` before it starts. Any value different from the job's ID opts out, including an empty string. On Linux and macOS a script can also set it before starting a process.
- Opted-out processes and descendants that inherit the override survive normal job cleanup and expired output waits. Detach helper output because the runner closes leftover output pipes after a bounded wait.
- Cancelling or timing out an active step still terminates its process tree.
- The override does not preserve workspace or temporary files. Store persistent helper files outside those directories and arrange the helper's eventual shutdown.

This follows [actions/runner's tracking ID and orphan cleanup](https://github.com/actions/runner/blob/main/src/Runner.Worker/JobExtension.cs). Container jobs follow their container's lifecycle.
