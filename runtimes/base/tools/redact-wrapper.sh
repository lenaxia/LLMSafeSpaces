#!/bin/sh
# Design 0053 S2: redact is now a workspace-agentd subcommand; this wrapper
# preserves the documented /usr/local/bin/redact path (docs/reference/cli.md,
# smoke-test.sh) with zero bytes of a second platform executable in the image.
# The supervisor additionally installs a fresh wrapper at
# /sandbox-runtime/bin/redact (uid-1000 tmpfs) that leads PATH for opencode
# children. This baked copy is the pre-S3 bridge and dies with the base strip.
exec /usr/local/bin/workspace-agentd redact "$@"
