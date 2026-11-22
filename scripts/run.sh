#!/usr/bin/env bash

mkdir -p /data
cd /data || exit 1

runner_args=(--config "$CONFIG_FILE")
RUNNER_STATE_FILE=${RUNNER_STATE_FILE:-$(gitea-runner "${runner_args[@]}" config runner-file)}

# Use the same ENV variable names as https://github.com/vegardit/docker-gitea-act-runner
test -f "$RUNNER_STATE_FILE" || echo "$RUNNER_STATE_FILE is missing or not a regular file"

if [[ ! -s "$RUNNER_STATE_FILE" ]]; then
  register_args=("${runner_args[@]}" --instance "$GITEA_INSTANCE_URL" --name "$GITEA_RUNNER_NAME" --labels "$GITEA_RUNNER_LABELS"
    --no-interactive ${GITEA_RUNNER_EPHEMERAL:+--ephemeral})
  if [[ -z $GITEA_RUNNER_REGISTRATION_TOKEN && -f $GITEA_RUNNER_REGISTRATION_TOKEN_FILE ]]; then
    register_args+=(--token-file "$GITEA_RUNNER_REGISTRATION_TOKEN_FILE")
  fi
  for ((try = 1; ; try++)); do # waits for Gitea like the daemon does, a container may have no restart policy
    if gitea-runner register "${register_args[@]}" 2>&1; then
      echo "SUCCESS"
      break
    fi
    if [[ ${GITEA_MAX_REG_ATTEMPTS:-0} -gt 0 && $try -ge $GITEA_MAX_REG_ATTEMPTS ]]; then
      echo "Registration failed after ${try} attempts"
      exit 1
    fi
    echo "Waiting to retry ..."
    sleep 5
  done
fi
# Prevent reading the token from the gitea-runner process
unset GITEA_RUNNER_REGISTRATION_TOKEN GITEA_RUNNER_REGISTRATION_TOKEN_FILE

exec gitea-runner daemon "${runner_args[@]}" ${GITEA_RUNNER_ONCE:+--once}
