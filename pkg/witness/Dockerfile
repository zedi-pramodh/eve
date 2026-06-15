# syntax=docker/dockerfile-upstream:1.5.0-rc2-labs
#
# Copyright (c) 2026 Zededa, Inc.
# SPDX-License-Identifier: Apache-2.0
#
# pkg/witness — 3rd etcd vote for 2-node EVE-k HA clusters.
# Mirrors the layout of pkg/kube but ships only what a witness needs:
# k3s itself plus etcdctl for membership maintenance. No KubeVirt, CDI,
# Longhorn, Multus, eve-bridge, kube-vip or descheduler — the witness
# never runs workloads.

FROM lfedge/eve-alpine:325b12087b386811fdb9d3c0adffa5fb94a34b95 AS build
ARG TARGETARCH
ENV PKGS alpine-baselayout musl-utils iproute2 iptables curl openrc \
         util-linux grep findutils jq inotify-tools
RUN eve-alpine-deploy.sh

ENV ETCDCTL_VERSION=v3.5.5
ADD https://github.com/etcd-io/etcd/releases/download/${ETCDCTL_VERSION}/etcd-${ETCDCTL_VERSION}-linux-${TARGETARCH}.tar.gz /tmp/etcd.tar.gz
RUN mkdir -p /out/usr/bin /tmp/etcdtmp && \
    tar -xzf /tmp/etcd.tar.gz -C /tmp/etcdtmp && \
    cp "/tmp/etcdtmp/etcd-${ETCDCTL_VERSION}-linux-${TARGETARCH}/etcdctl" /out/usr/bin/etcdctl && \
    chmod +x /out/usr/bin/etcdctl && \
    rm -rf /tmp/etcd.tar.gz /tmp/etcdtmp

# Register etcdctl in the APK DB, mandatory for it to be included in the SBOM.
RUN register-sbom-pkg.sh -n etcdctl -v "${ETCDCTL_VERSION}" -l Apache-2.0 -u https://github.com/etcd-io/etcd

FROM scratch
COPY --from=build /out/ /

# Witness-specific config layout: mirrors /etc/rancher/k3s used by pkg/kube
# so that future cluster join logic can drop config fragments into the same
# directory.
RUN mkdir -p /etc/rancher/k3s/config.yaml.d
RUN mkdir -p /etc/witness

COPY config.yaml /etc/rancher/k3s/config.yaml
COPY 00-nodename.yaml /etc/rancher/k3s/config.yaml.d/00-nodename.yaml

COPY witness-init.sh /usr/bin/witness-init.sh
COPY witness-utils.sh /usr/bin/witness-utils.sh
COPY lib/config.sh /usr/bin/witness-config.sh
COPY wipe-and-rejoin.sh /usr/bin/wipe-and-rejoin.sh
RUN chmod +x /usr/bin/witness-init.sh /usr/bin/witness-utils.sh /usr/bin/wipe-and-rejoin.sh

# Witness-private containerd config (kubelet talks to it via
# /run/witness/containerd/containerd.sock). See containerd-config.toml
# for why every path here differs from pkg/kube's /run/containerd-user/.
COPY containerd-config.toml /etc/witness/containerd-config.toml

ENTRYPOINT []
CMD ["/usr/bin/witness-init.sh"]
