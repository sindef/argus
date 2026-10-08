VERSION ?= 0.1.0
IMAGE ?= argus-os:$(VERSION)
AURORABOOT ?= quay.io/kairos/auroraboot:v0.28.0

.PHONY: os iso

os:
	docker buildx build --load --build-arg VERSION=$(VERSION) -t $(IMAGE) os

iso: os
	docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v $(CURDIR)/build:/output \
		$(AURORABOOT) build-iso --output /output/ oci:$(IMAGE)
