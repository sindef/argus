ARG KAIROS_INIT_VERSION=v4.3.0

FROM quay.io/kairos/kairos-init:${KAIROS_INIT_VERSION} AS kairos-init

FROM ubuntu:26.04
ARG VERSION=0.1.0
ARG K3S_VERSION=v1.35.8+k3s1
ARG KUBEVIRT_VERSION=v1.9.0
ARG CDI_VERSION=v1.66.1
ARG NAD_VERSION=v1.7.7
ARG SNAPSHOTTER_VERSION=v8.6.0

RUN --mount=type=bind,from=kairos-init,src=/kairos-init,dst=/kairos-init \
    /kairos-init -s install -m generic -p k3s --provider-k3s-version "${K3S_VERSION}" --version "${VERSION}"

RUN apt-get update \
    && apt-get install -y --no-install-recommends zfsutils-linux \
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
    && rm -rf /var/lib/apt/lists/*

ADD https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}/kubevirt-operator.yaml /usr/share/argus/manifests/10-kubevirt-operator.yaml
ADD https://github.com/kubevirt/containerized-data-importer/releases/download/${CDI_VERSION}/cdi-operator.yaml /usr/share/argus/manifests/10-cdi-operator.yaml
ADD https://raw.githubusercontent.com/k8snetworkplumbingwg/network-attachment-definition-client/${NAD_VERSION}/artifacts/networks-crd.yaml /usr/share/argus/manifests/10-nad-crd.yaml
ADD https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/${SNAPSHOTTER_VERSION}/deploy/kubernetes/snapshot-controller/rbac-snapshot-controller.yaml /usr/share/argus/manifests/20-snapshot-controller-rbac.yaml
ADD https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/${SNAPSHOTTER_VERSION}/deploy/kubernetes/snapshot-controller/setup-snapshot-controller.yaml /usr/share/argus/manifests/20-snapshot-controller.yaml
RUN cd /usr/share/argus/manifests \
    && sed -i 's/^  replicas: 2$/  replicas: 1/; s/^            memory: 450Mi$/            memory: 256Mi/' 10-kubevirt-operator.yaml \
    && sed -i 's/^            memory: 150Mi$/            memory: 80Mi/' 10-cdi-operator.yaml \
    && grep -q '^            memory: 256Mi$' 10-kubevirt-operator.yaml \
    && grep -q '^            memory: 80Mi$' 10-cdi-operator.yaml \
    && sed -i 's/^  replicas: 2$/  replicas: 1/; s/--leader-election=true/--leader-election=false/; s/--v=5/--v=2/' 20-snapshot-controller.yaml \
    && grep -q -- '--leader-election=false' 20-snapshot-controller.yaml
COPY rootfs/ /
COPY images/ /usr/share/argus/images/
RUN sed -i "s/argus:VERSION/argus:${VERSION}/" /usr/share/argus/manifests/40-argus.yaml

RUN --mount=type=bind,from=kairos-init,src=/kairos-init,dst=/kairos-init \
    /kairos-init -s init -m generic -p k3s --provider-k3s-version "${K3S_VERSION}" --version "${VERSION}"
