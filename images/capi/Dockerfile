# syntax=docker/dockerfile:1.4

# Copyright 2020 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

ARG BASE_IMAGE=docker.io/library/ubuntu:24.04
FROM $BASE_IMAGE

ARG TAG=dev
LABEL org.opencontainers.image.source=https://github.com/kubernetes-sigs/image-builder
LABEL org.opencontainers.image.url=https://image-builder.sigs.k8s.io
LABEL org.opencontainers.image.version=$TAG

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y \
	apt-transport-https \
	build-essential \
	ca-certificates \
	# createrepo-c: harvesting builds write the dnf metadata of the OS package
	# bundles they make (dpkg-scanpackages, for apt, comes with build-essential).
	createrepo-c \
	curl \
	git \
	jq \
	python3-pip \
	rsync \
	unzip \
	vim \
	wget \
	qemu-system-x86 \
	qemu-kvm \
	# Adding xorriso to create iso for mounting cd_drives which then can be used for bootstrapping node image
	xorriso \
	&& useradd -ms /bin/bash imagebuilder \
	&& apt-get purge --auto-remove -y \
	&& rm -rf /var/lib/apt/lists/*

ARG ARCH
ARG PASSED_IB_VERSION

# Bring-your-own Packer build args. Defaults match hack/ensure-packer.sh,
# which pins Packer to the last MPL-2.0 release. Set PACKER_VERSION to
# install a different version, or PACKER_BIN at runtime to use an
# already-installed binary. See docs/book/src/capi/packer.md.
ARG PACKER_VERSION=1.9.5
ARG IB_ALLOW_ANY_PACKER=0

USER imagebuilder
WORKDIR /home/imagebuilder/

COPY --chown=imagebuilder:imagebuilder ansible ansible/
COPY --chown=imagebuilder:imagebuilder ansible.cfg ansible.cfg
COPY --chown=imagebuilder:imagebuilder cloudinit cloudinit/
COPY --chown=imagebuilder:imagebuilder hack hack/
COPY --chown=imagebuilder:imagebuilder packer packer/
COPY --chown=imagebuilder:imagebuilder Makefile Makefile
COPY --chown=imagebuilder:imagebuilder azure_targets.sh azure_targets.sh

ENV PATH="/home/imagebuilder/.local/bin:${PATH}"
ENV PACKER_ARGS=''
ENV PACKER_VAR_FILES=''
ENV PACKER_VERSION="${PACKER_VERSION}"
ENV IB_ALLOW_ANY_PACKER="${IB_ALLOW_ANY_PACKER}"
ENV IB_VERSION="${PASSED_IB_VERSION}"
# Ubuntu 24.04's OpenSSL switches to FIPS mode by itself when the kernel has
# FIPS enabled (/proc/sys/crypto/fips_enabled, which a container shares with
# the node it runs on), and this image has no FIPS provider. On a FIPS
# Kubernetes node every OpenSSL client in the build pod then fails: ssh exits
# with "PRNG is not seeded" and Ansible reports the build VM UNREACHABLE.
# Keep the builder's own tools out of FIPS mode. (Its ssh only talks to
# Packer's local proxy; FIPS images get their FIPS setup from the build.)
ENV OPENSSL_FORCE_FIPS_MODE=0

RUN make deps

# Tools the build uploads to the VM instead of downloading them there, so a
# build needs no internet access from the VM (air-gapped builds need none at
# all): goss for the goss step, and mindthegap to load NKP's image bundles
# into containerd. linux/amd64 whatever this image's architecture, as that's
# what the VM runs. goss's version must match goss_version.
ARG GOSS_VERSION=0.3.23
ARG GOSS_SHA256=9e9f24e25f86d6adf2e669a9ffbe8c3d7b9b439f5f877500dea02ba837e10e4d
ARG MINDTHEGAP_VERSION=1.26.3
ARG MINDTHEGAP_SHA256=824c9f997f461e71d4b4fb9e6bb37c7f7b1234a3a08c96de73360714bf9acf3a
ENV IB_VM_TOOLS_DIR=/home/imagebuilder/vm-tools
RUN mkdir -p "${IB_VM_TOOLS_DIR}" && cd "${IB_VM_TOOLS_DIR}" \
	&& curl -fsSL -o "goss-${GOSS_VERSION}-linux-amd64" \
		"https://github.com/goss-org/goss/releases/download/v${GOSS_VERSION}/goss-linux-amd64" \
	&& echo "${GOSS_SHA256}  goss-${GOSS_VERSION}-linux-amd64" | sha256sum -c - \
	&& curl -fsSL -o mindthegap.tar.gz \
		"https://github.com/mesosphere/mindthegap/releases/download/v${MINDTHEGAP_VERSION}/mindthegap_v${MINDTHEGAP_VERSION}_linux_amd64.tar.gz" \
	&& echo "${MINDTHEGAP_SHA256}  mindthegap.tar.gz" | sha256sum -c - \
	&& tar -xzf mindthegap.tar.gz mindthegap && mv mindthegap mindthegap-linux-amd64 && rm mindthegap.tar.gz \
	&& chmod 0755 "goss-${GOSS_VERSION}-linux-amd64" mindthegap-linux-amd64

ENTRYPOINT [ "/usr/bin/make" ]
