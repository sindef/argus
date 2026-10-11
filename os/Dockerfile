ARG KAIROS_INIT_VERSION=v4.3.0

FROM quay.io/kairos/kairos-init:${KAIROS_INIT_VERSION}@sha256:c8c5d46234ddda93908f090f136fb12826e312286f63e0b6f316e4063edeea53 AS kairos-init

FROM ubuntu:26.04@sha256:f144425ff09be612d6d9ad965196e9cdc23dae1f42110a8a11a3e9a8198759f7 AS build
ARG VERSION=0.1.0
ARG K3S_VERSION=v1.35.8+k3s1
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
COPY rootfs/ /
COPY images/ /usr/share/argus/images/
COPY build/argus-hostd /usr/sbin/argus-hostd
RUN sed -i "s/argus:VERSION/argus:${VERSION}/" /usr/share/argus/manifests/40-argus.yaml

RUN --mount=type=bind,from=kairos-init,src=/kairos-init,dst=/kairos-init \
    /kairos-init -s init -m generic -p k3s --provider-k3s-version "${K3S_VERSION}" --version "${VERSION}"

RUN systemctl disable fail2ban && dpkg --purge fail2ban && rm -rf /etc/fail2ban

FROM scratch
COPY --from=build / /
