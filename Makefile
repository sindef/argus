VERSION ?= 0.1.0
IMAGE ?= argus-os:$(VERSION)
AURORABOOT ?= quay.io/kairos/auroraboot:v0.28.0

.PHONY: app manager os iso

app:
	docker buildx build --load -t argus:$(VERSION) .
	mkdir -p os/images
	docker save -o os/images/argus.tar argus:$(VERSION)

manager:
	docker buildx build --load --build-arg CMD=argus-manager -t argus-manager:$(VERSION) .

os: app
	docker buildx build --load --build-arg VERSION=$(VERSION) -t $(IMAGE) os

iso: os
	docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v $(CURDIR)/build:/output \
		$(AURORABOOT) build-iso --output /output/ oci:$(IMAGE)
