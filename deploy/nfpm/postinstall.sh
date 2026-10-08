#!/bin/sh
# Post-install hook for the kelvran-gateway deb/rpm/apk (gateway/.goreleaser.yaml).
# Deliberately minimal: reload systemd so the freshly installed unit is
# known, and restart the service only if it is already running (an upgrade).
# The package never enables or starts the service
# -- the gateway has no usable default config, and an operator must create
# /etc/kelvran-gateway/config.yaml first (see the unit file's header).
# Tolerates hosts without systemd (containers, Alpine with OpenRC): the
# command is simply skipped.
set -eu
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload >/dev/null 2>&1 || true
  # On an upgrade, a RUNNING service picks up the new binary; a stopped or
  # never-enabled one stays exactly as it was (try-restart never starts).
  systemctl try-restart kelvran-gateway.service >/dev/null 2>&1 || true
fi
exit 0
