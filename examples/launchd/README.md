# Running the runner as a launchd service on macOS

[`com.gitea.runner.plist`](./com.gitea.runner.plist) is an example job for
running the runner as a background service on macOS.

## Setup

1. Install the `gitea-runner` binary (e.g. to `/usr/local/bin/gitea-runner`).
2. Register as the user that jobs should run as, with a host label so that jobs
   run on the Mac itself:

   ```bash
   mkdir ~/gitea-runner && cd ~/gitea-runner
   gitea-runner register --instance https://gitea.example.com --token <token> --labels macos-arm64:host
   ```

3. Replace `runner` in the job with that user, then install and start it:

   ```bash
   sudo cp com.gitea.runner.plist /Library/LaunchDaemons/
   sudo launchctl bootstrap system /Library/LaunchDaemons/com.gitea.runner.plist
   ```

launchd passes no shell environment to the runner. Set what jobs need, such as
proxy variables, in `EnvironmentVariables`.

## Jobs that need the login session

A daemon starts at boot, and its jobs have no login session. If jobs need the
login keychain or the GUI, for example for UI tests, run the runner as an agent
instead. Remove the `UserName` key, enable automatic login for the user and
install the job as that user:

```bash
mkdir -p ~/Library/LaunchAgents
cp com.gitea.runner.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.gitea.runner.plist
```

Two things to know when running as an agent:

- [Local network privacy](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy)
  keeps an agent from reaching other hosts on the Mac's own subnet, such as a
  shared cache server, with `no route to host`. On macOS 15.5 or later, exempt
  the subnet and restart the Mac. On Wi-Fi, the key is
  `AllowedWiFiLocalNetworkAddresses`:

  ```bash
  sudo defaults write com.apple.network.local-network AllowedEthernetLocalNetworkAddresses -array "192.168.1.0/24"
  ```

- Git on macOS saves credentials from remote URLs to the keychain, which can
  stop a job at a dialog that nobody answers. Set `GIT_CONFIG_NOSYSTEM` to `1`
  in `EnvironmentVariables` to turn that off.
