# syntax=docker/dockerfile-upstream:1.5.0-rc2-labs

FROM lfedge/eve-alpine:6db79cac4bacb88e31189721f02029305f032cae as build
ENV PKGS alpine-baselayout musl-utils iproute2 iptables curl openrc \
	open-iscsi libvirt libvirt-client
RUN eve-alpine-deploy.sh

FROM scratch
COPY --from=build /out/ /
COPY cluster-init.sh /usr/bin/
COPY cgconfig.conf /etc
WORKDIR /

ENV K3S_VERSION v1.25.3+k3s1

RUN /usr/bin/curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=${K3S_VERSION} INSTALL_K3S_SKIP_ENABLE=true INSTALL_K3S_BIN_DIR=/usr/bin sh -

ENTRYPOINT []
CMD ["/usr/bin/cluster-init.sh"]
