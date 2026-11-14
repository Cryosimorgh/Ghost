#!/bin/sh
set -eu
cd -- "$(dirname -- "$0")"
exec ./Ghost-linux-amd64 -open=true "$@"
