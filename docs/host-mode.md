# Host mode

## Stable workspace

Host jobs run in a random `<host.workdir_parent>/<16 hex>/hostexecutor` directory. With `host.stable_workdir: true`, Linux host jobs run in `<host.workdir_parent>/workspaces/<owner>/<repo>/<repo>` instead, like GitHub's `_work/<repo>/<repo>`. This needs `CAP_SYS_ADMIN`, so run the runner as root or with `AmbientCapabilities=CAP_SYS_ADMIN` in its systemd unit. Steps don't get the capability. Without it, jobs keep the random directory and log a warning.

Concurrent jobs of a repository use the same path, each with its own files, through a private mount namespace. On the host, the path shows the files of one of them, so processes started outside a job, like containers from `docker run -v "$PWD:/src"` or systemd services, only see the right files there in that job. Every job starts with an empty workspace.

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
- Cancelling or timing out an active step still terminates its process tree, on Unix with SIGINT, then SIGTERM after 7.5 seconds and SIGKILL after 2.5 more.
- The override does not preserve workspace or temporary files. Store persistent helper files outside those directories and arrange the helper's eventual shutdown.

This follows [actions/runner's tracking ID and orphan cleanup](https://github.com/actions/runner/blob/main/src/Runner.Worker/JobExtension.cs). Container jobs follow their container's lifecycle.
