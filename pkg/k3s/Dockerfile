# syntax=docker/dockerfile-upstream:1.5.0-rc2-labs

FROM lfedge/eve-alpine:12496cf02e1ee62d5654782f313af3ec365a9af1 as build
ENV PKGS alpine-baselayout musl-utils iproute2 iptables curl openrc \
	open-iscsi libvirt libvirt-client
RUN eve-alpine-deploy.sh

FROM scratch
COPY --from=build /out/ /
COPY cluster-init.sh /usr/bin/
COPY cgconfig.conf /etc

WORKDIR /
ENTRYPOINT []
CMD ["/usr/bin/cluster-init.sh"]
