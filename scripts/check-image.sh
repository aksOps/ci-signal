#!/bin/sh
set -eu

image=${1:-ci-signal:test}

arch=$(docker image inspect --format '{{.Architecture}}' "$image")
test "$arch" = amd64

docker run --rm --entrypoint /bin/sh "$image" -ceu '
  test "$(id -u)" = 10001
  test "$(id -g)" = 10001
  test -w /var/lib/ci-signal/copilot
  test -w /var/lib/ci-signal/state
  test "$(stat -c %a /var/lib/ci-signal/copilot)" = 700
  test "$(stat -c %a /var/lib/ci-signal/state)" = 700
  test ! -w /opt/ci-signal/core
  test ! -w /opt/copilot
  test -r /opt/ci-signal/examples/reviewer.json
  test -r /opt/ci-signal/core/skills/review/SKILL.md
  test -r /opt/ci-signal/core/skills/ast-analysis/SKILL.md
  test -r /opt/ci-signal/core/skills/ast-analysis/rules/go-declarations.yml
  test -r /opt/ci-signal/core/instructions/reviewer.md
  test -r /opt/ci-signal/core/prompts/review.md
  test -x "$COPILOT_CLI_PATH"
  test -r "$(dirname "$COPILOT_CLI_PATH")/runtime.node"
  echo "580f45a5dca10be9122180bce579055be25123efd348f8ed40ffd3e00aaf2044  $COPILOT_CLI_PATH" | sha256sum -c -
  echo "79f649256bdb76f448c6804cc7165eea3ba90b7773ace6758aeb77518125fbd8  $(dirname "$COPILOT_CLI_PATH")/runtime.node" | sha256sum -c -
  echo "b9680937e10425bf19862908856c16ed274e6b81a1368bd62be7a757eab21628  /opt/copilot/LICENSE.md" | sha256sum -c -
  echo "7a5ab30160186184c0bf8bffc87da4af25123c183964cd98c11b0b354137db0a  /usr/local/bin/ast-grep" | sha256sum -c -
  git --version | grep -F "git version 2.39.5"
  ast-grep --version | grep -Fx "ast-grep 0.45.3"
'

docker run --rm --entrypoint /usr/local/bin/ci-signal "$image" --help >/dev/null
docker run --rm --workdir /tmp --entrypoint /usr/local/bin/ci-signal "$image" --help >/dev/null
