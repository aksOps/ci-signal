# syntax=docker/dockerfile:1.7

FROM golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd AS build

ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN go version | grep -Fx 'go version go1.26.5 linux/amd64' \
 && go build -buildvcs=false -trimpath -ldflags='-s -w' -o /out/ci-signal ./cmd/ci-signal

FROM build AS tools

ARG DEBIAN_SNAPSHOT=20260824T000000Z
ARG COPILOT_VERSION=1.0.83
ARG COPILOT_SHA256=888f8fbb4575c335afba4a8863c647ef04f81e5124c7c794bdcaee90c5fa4503
ARG RTK_VERSION=0.48.0
ARG RTK_SHA256=e4e650fa1677c0de2f6839a6040d7b17f312d32f163c402b75af70e9e5af1a91
ARG RTK_LICENSE_SHA256=4044ade9c21d8b084d3d16a03375cf3b7e166b946a327bb37a3fbbdb53287cfd
ARG AST_GREP_VERSION=0.45.3
ARG AST_GREP_SHA256=f8ac830881339d1edee6b2652f54798c0f4da5a827f2db38a08ee31117783ce8
ARG AST_GREP_LICENSE_SHA256=81471889c77b2161a3e4dcdb1b2e6ca382e485766132d92d5fe1d7497e7dd2d9

RUN printf '%s\n' \
      "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/ bookworm main" \
      "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian-security/${DEBIAN_SNAPSHOT}/ bookworm-security main" \
      "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/ bookworm-updates main" \
      > /etc/apt/sources.list \
 && rm -f /etc/apt/sources.list.d/debian.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends unzip=6.0-28+deb12u1 \
 && rm -rf /var/lib/apt/lists/*

RUN curl --fail --location --proto '=https' --tlsv1.2 \
      "https://github.com/github/copilot-cli/releases/download/v${COPILOT_VERSION}/github-copilot-${COPILOT_VERSION}-linux-x64.tgz" \
      --output /tmp/copilot.tgz \
 && echo "${COPILOT_SHA256}  /tmp/copilot.tgz" | sha256sum --check --strict \
 && mkdir -p /opt/copilot \
 && tar -xzf /tmp/copilot.tgz -C /opt/copilot --strip-components=1 \
 && test -x /opt/copilot/prebuilds/linux-x64/copilot-runtime \
 && test -r /opt/copilot/prebuilds/linux-x64/runtime.node \
 && test -r /opt/copilot/LICENSE.md \
 && rm /tmp/copilot.tgz

RUN curl --fail --location --proto '=https' --tlsv1.2 \
      "https://github.com/ast-grep/ast-grep/releases/download/${AST_GREP_VERSION}/app-x86_64-unknown-linux-gnu.zip" \
      --output /tmp/ast-grep.zip \
 && echo "${AST_GREP_SHA256}  /tmp/ast-grep.zip" | sha256sum --check --strict \
 && unzip -q /tmp/ast-grep.zip -d /tmp/ast-grep \
 && install -m 0755 /tmp/ast-grep/ast-grep /out/ast-grep \
 && curl --fail --location --proto '=https' --tlsv1.2 \
      "https://raw.githubusercontent.com/ast-grep/ast-grep/${AST_GREP_VERSION}/LICENSE" \
      --output /out/ast-grep.LICENSE \
 && echo "${AST_GREP_LICENSE_SHA256}  /out/ast-grep.LICENSE" | sha256sum --check --strict \
 && rm -rf /tmp/ast-grep /tmp/ast-grep.zip

RUN curl --fail --location --proto '=https' --tlsv1.2 \
      "https://github.com/rtk-ai/rtk/releases/download/v${RTK_VERSION}/rtk-x86_64-unknown-linux-musl.tar.gz" \
      --output /tmp/rtk.tar.gz \
 && echo "${RTK_SHA256}  /tmp/rtk.tar.gz" | sha256sum --check --strict \
 && mkdir /tmp/rtk \
 && tar -xzf /tmp/rtk.tar.gz -C /tmp/rtk \
 && install -m 0755 /tmp/rtk/rtk /out/rtk \
 && curl --fail --location --proto '=https' --tlsv1.2 \
      "https://raw.githubusercontent.com/rtk-ai/rtk/v${RTK_VERSION}/LICENSE" \
      --output /out/rtk.LICENSE \
 && echo "${RTK_LICENSE_SHA256}  /out/rtk.LICENSE" | sha256sum --check --strict \
 && COPILOT_HOME=/out/rtk-copilot /out/rtk init --global --copilot \
 && rm -rf /tmp/rtk /tmp/rtk.tar.gz

FROM debian:bookworm-20260824-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

ARG DEBIAN_SNAPSHOT=20260824T000000Z

RUN printf '%s\n' \
      "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/ bookworm main" \
      "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian-security/${DEBIAN_SNAPSHOT}/ bookworm-security main" \
      "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/ bookworm-updates main" \
      > /etc/apt/sources.list \
 && rm -f /etc/apt/sources.list.d/debian.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates=20250419~deb12u1 \
      git=1:2.39.5-0+deb12u3 \
      libgcc-s1=12.2.0-14+deb12u1 \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build --chown=0:0 /out/ci-signal /usr/local/bin/ci-signal
COPY --from=tools --chown=0:0 /out/ast-grep /usr/local/bin/ast-grep
COPY --from=tools --chown=0:0 /out/ast-grep.LICENSE /opt/licenses/ast-grep.LICENSE
COPY --from=tools --chown=0:0 /out/rtk /usr/local/bin/rtk
COPY --from=tools --chown=0:0 /out/rtk.LICENSE /opt/licenses/rtk.LICENSE
COPY --from=tools --chown=0:0 /out/rtk-copilot /opt/ci-signal/rtk
COPY --from=tools --chown=0:0 /opt/copilot /opt/copilot
COPY --chown=0:0 core /opt/ci-signal/core
COPY --chown=0:0 examples /opt/ci-signal/examples

RUN install -d -m 0700 -o 10001 -g 10001 \
      /var/lib/ci-signal /var/lib/ci-signal/.copilot /var/lib/ci-signal/copilot /var/lib/ci-signal/state /workspace \
 && cp -R /opt/ci-signal/rtk/. /var/lib/ci-signal/.copilot/ \
 && chown -R 10001:10001 /var/lib/ci-signal/.copilot \
 && chmod -R a+rX /opt/ci-signal/rtk \
 && chmod -R a-w /opt/copilot /opt/ci-signal /opt/licenses \
 && test -x /usr/local/bin/ci-signal \
 && test -x /usr/local/bin/ast-grep \
 && test -x /usr/local/bin/rtk

ENV HOME=/var/lib/ci-signal \
    PATH=/usr/local/bin:/usr/bin:/bin \
    COPILOT_CLI_PATH=/opt/copilot/prebuilds/linux-x64/copilot-runtime

USER 10001:10001
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/ci-signal"]
