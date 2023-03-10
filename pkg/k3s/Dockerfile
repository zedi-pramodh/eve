# syntax=docker/dockerfile-upstream:1.5.0-rc2-labs

FROM lfedge/eve-alpine:ae40dd538dde603e77e0eeb2fb1c79c633ea73c7 as build
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
