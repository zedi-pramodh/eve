# syntax=docker/dockerfile-upstream:1.5.0-rc2-labs

FROM lfedge/eve-alpine:40d9ccdde616ddd819cc814f0b12f7feea379abb as build
ENV PKGS alpine-baselayout musl-utils iproute2 iptables curl openrc open-iscsi
RUN eve-alpine-deploy.sh


FROM scratch
COPY --from=build /out/ /
COPY cluster-init.sh /usr/bin/
COPY cgconfig.conf /etc

WORKDIR /
ENTRYPOINT []
CMD ["/usr/bin/cluster-init.sh"]
