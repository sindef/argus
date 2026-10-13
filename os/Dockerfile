ARG KAIROS_INIT_VERSION=v4.3.0
ARG K3S_VERSION=v1.35.8+k3s1
ARG NOTICES=none

FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS infrastructure-full
ARG KAIROS_INIT_VERSION
ARG K3S_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends jq && rm -rf /var/lib/apt/lists/*
COPY infrastructure-sources.tsv /infrastructure-sources.tsv
COPY infrastructure-licenses /infrastructure-licenses
COPY third-party-infrastructure.sh /third-party-infrastructure.sh
RUN --mount=type=cache,target=/go/pkg/mod sh /third-party-infrastructure.sh

FROM --platform=$BUILDPLATFORM ubuntu:26.04@sha256:f144425ff09be612d6d9ad965196e9cdc23dae1f42110a8a11a3e9a8198759f7 AS k3s-root-notices-full
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates make gcc g++ rsync wget patch file perl unzip cpio bc bzip2 xz-utils m4 && rm -rf /var/lib/apt/lists/*
COPY --from=infrastructure-full /upstream/k3s-root/ /usr/src/k3s-root/
COPY --from=infrastructure-full /upstream/buildroot/ /usr/src/buildroot/
WORKDIR /usr/src/k3s-root
ENV BUILDARCH=${TARGETARCH} FORCE_UNSAFE_CONFIGURE=1
RUN --mount=type=cache,target=/usr/src/buildroot/dl scripts/patch && cd /usr/src/buildroot && make olddefconfig \
    && make legal-info BR2_PRIMARY_SITE=https://sources.buildroot.net BR2_WGET='wget --passive-ftp -nd -t 3 -T 30' \
    && rm -rf output/build output/per-package

FROM --platform=$BUILDPLATFORM ubuntu:26.04@sha256:f144425ff09be612d6d9ad965196e9cdc23dae1f42110a8a11a3e9a8198759f7 AS notices-none
RUN mkdir -p /notices /source-bundle /usr/src/buildroot/output/legal-info/licenses \
    && touch /usr/src/buildroot/output/legal-info/manifest.csv

FROM notices-none AS infrastructure-none
FROM notices-none AS k3s-root-notices-none
FROM infrastructure-${NOTICES} AS infrastructure
FROM k3s-root-notices-${NOTICES} AS k3s-root-notices

FROM quay.io/kairos/kairos-init:${KAIROS_INIT_VERSION}@sha256:c8c5d46234ddda93908f090f136fb12826e312286f63e0b6f316e4063edeea53 AS kairos-init

FROM scratch AS cosign-amd64
ADD --chmod=755 --checksum=sha256:4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71 https://github.com/sigstore/cosign/releases/download/v3.1.3/cosign-linux-amd64 /cosign

FROM scratch AS cosign-arm64
ADD --chmod=755 --checksum=sha256:c5d324e091826b0d7a78eb16fef316450b4eb9aaec045611c08ba06f5e73220a https://github.com/sigstore/cosign/releases/download/v3.1.3/cosign-linux-arm64 /cosign

FROM cosign-${TARGETARCH} AS cosign

FROM ubuntu:26.04@sha256:f144425ff09be612d6d9ad965196e9cdc23dae1f42110a8a11a3e9a8198759f7 AS build
ARG VERSION=0.1.0
ARG K3S_VERSION
ARG KUBEVIRT_VERSION=v1.9.0
ARG CDI_VERSION=v1.66.1
ARG NAD_VERSION=v1.7.7

RUN --mount=type=bind,from=kairos-init,src=/kairos-init,dst=/kairos-init \
    /kairos-init -s install -m generic -p k3s --provider-k3s-version "${K3S_VERSION}" --version "${VERSION}"

RUN apt-get update \
    && apt-get install -y --no-install-recommends zfsutils-linux smartmontools rsyslog \
    && apt-get install -y --allow-remove-essential coreutils-from-gnu coreutils-from-uutils- \
    && dpkg --purge --force-depends rust-coreutils \
    && apt-get purge -y --auto-remove \
        linux-firmware \
        linux-firmware-amd-graphics \
        linux-firmware-broadcom-wireless \
        linux-firmware-intel-graphics \
        linux-firmware-intel-wireless \
        linux-firmware-marvell-prestera \
        linux-firmware-marvell-wireless \
        linux-firmware-mediatek \
        linux-firmware-mellanox-spectrum \
        linux-firmware-nvidia-graphics \
        linux-firmware-qualcomm-graphics \
        linux-firmware-qualcomm-misc \
        linux-firmware-qualcomm-wireless \
        neovim \
        neovim-runtime \
    && rm -rf /var/lib/apt/lists/* \
    && systemctl mask iscsid.socket iscsid.service apt-daily.timer apt-daily-upgrade.timer motd-news.timer dpkg-db-backup.timer ua-timer.timer

ADD --checksum=sha256:f11307caafc3c23ffedf9887d8beb5a4419e2694da242fa68f63d1ec820de2e0 https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}/kubevirt-operator.yaml /usr/share/argus/manifests/10-kubevirt-operator.yaml
ADD --checksum=sha256:c7d92bd1b2ee1a3952a5990000437c08d58109f31c0bd48ab6188ec27a13e224 https://github.com/kubevirt/containerized-data-importer/releases/download/${CDI_VERSION}/cdi-operator.yaml /usr/share/argus/manifests/10-cdi-operator.yaml
ADD --checksum=sha256:2c83987cbc34c16e7b49921237c39151008e9239304ef2dc7163b92931ef1880 https://raw.githubusercontent.com/k8snetworkplumbingwg/network-attachment-definition-client/${NAD_VERSION}/artifacts/networks-crd.yaml /usr/share/argus/manifests/10-nad-crd.yaml
RUN cd /usr/share/argus/manifests \
    && sed -i 's/^  replicas: 2$/  replicas: 1/; s/^            memory: 450Mi$/            memory: 256Mi/' 10-kubevirt-operator.yaml \
    && sed -i 's/^            memory: 150Mi$/            memory: 80Mi/' 10-cdi-operator.yaml \
    && sed -i 's/^        env:$/        env:\n        - name: GOMEMLIMIT\n          value: 160MiB\n        - name: GOGC\n          value: "50"/' 10-kubevirt-operator.yaml \
    && sed -i 's/^      - env:$/      - env:\n        - name: GOMEMLIMIT\n          value: 64MiB\n        - name: GOGC\n          value: "50"/' 10-cdi-operator.yaml \
    && grep -q '^            memory: 256Mi$' 10-kubevirt-operator.yaml \
    && grep -q '^            memory: 80Mi$' 10-cdi-operator.yaml \
    && [ "$(grep -c 'name: GOMEMLIMIT' 10-kubevirt-operator.yaml)" = 1 ] \
    && [ "$(grep -c 'name: GOMEMLIMIT' 10-cdi-operator.yaml)" = 1 ]
COPY --from=cosign /cosign /usr/bin/cosign
COPY rootfs/ /
COPY images/ /usr/share/argus/images/
COPY build/argus-hostd /usr/sbin/argus-hostd
RUN sed -i "s/argus:VERSION/argus:${VERSION}/" /usr/share/argus/manifests/40-argus.yaml

RUN --mount=type=bind,from=kairos-init,src=/kairos-init,dst=/kairos-init \
    /kairos-init -s init -m generic -p k3s --provider-k3s-version "${K3S_VERSION}" --version "${VERSION}"

RUN systemctl disable fail2ban && dpkg --purge fail2ban && rm -rf /etc/fail2ban
COPY build/usr/share/doc/argus/ /usr/share/doc/argus/
COPY --from=infrastructure /notices/ /usr/share/doc/argus/third_party/infrastructure/
COPY --from=k3s-root-notices /usr/src/buildroot/output/legal-info/licenses/ /usr/share/doc/argus/third_party/licenses/k3s-root/
COPY --from=k3s-root-notices /usr/src/buildroot/output/legal-info/manifest.csv /usr/share/doc/argus/third_party/k3s-root-manifest.csv
COPY third-party-packages.sh /tmp/third-party-packages.sh
RUN sh /tmp/third-party-packages.sh && rm /tmp/third-party-packages.sh

FROM build AS sources
COPY --from=infrastructure /source-bundle/ /source-bundle/
COPY --from=k3s-root-notices /usr/src/buildroot/output/legal-info/ /source-bundle/k3s-root/
COPY third-party-sources.sh /tmp/third-party-sources.sh
RUN sh /tmp/third-party-sources.sh && rm /tmp/third-party-sources.sh

FROM scratch AS source-bundle
COPY --from=sources /source-bundle/ /

FROM scratch
COPY --from=build / /
