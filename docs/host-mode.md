# Host-mode background processes on Windows

Windows host jobs clean up the processes their steps leave running. Each job supplies a unique `RUNNER_TRACKING_ID` before step environment overrides are applied. By default, step processes join the job's kill-on-close Windows Job Object, and closing it at job completion terminates the remaining members. A fallback sweep checks processes in the runner's Windows session whose executable, command line or working directory references a job-owned directory. The sweep only terminates a process whose environment still carries that job's tracking ID.

To start a helper that should outlive normal job completion, clear the variable in the **step's `env`**:

```yaml
steps:
  - name: Start shared helper
    env:
      RUNNER_TRACKING_ID: ""
    run: Start-Process vendor-helper.exe
```

Any value different from the job's ID opts the step out, including an empty string. Windows environment variable names and tracking ID comparisons are case insensitive. The step and the descendants it starts are excluded from the job's kill-on-close Job Object. Descendants that inherit the cleared or changed ID are also excluded from both workspace sweeps, even when their working directory is inside the workspace or their executable or command line references it. They are not terminated when a completed step's output wait expires either. Helpers should detach their output from the step because the runner closes leftover output pipes after a bounded wait.

Set the override on the step that starts the helper. Clearing the variable only inside an already running script cannot remove that script or its children from the Windows Job Object. An opted-out descendant that restores the original job ID can be reclaimed by the workspace sweep. The sweep skips processes whose environment cannot be read, and supports native processes and 32-bit processes inspected by a 64-bit runner.

This opt-out applies to normal completion cleanup. Cancelling or timing out an active step still terminates that step's process tree. It does not preserve workspace or temporary files, prevent operator-provided cleanup scripts from acting, or keep processes alive across runner host shutdown. Use a persistent directory outside the job's workspace and temporary directories for helpers shared by concurrent jobs, and arrange their eventual shutdown. A helper inside the workspace survives process cleanup, but its open handles may delay directory deletion and files it needs may be removed.

This follows [actions/runner's tracking ID and orphan cleanup](https://github.com/actions/runner/blob/main/src/Runner.Worker/JobExtension.cs), which sets a unique job ID and only kills new processes whose environment contains the matching value. The Windows Job Object in this runner additionally requires the override to be present when the step starts. Container jobs retain their container's lifecycle.
