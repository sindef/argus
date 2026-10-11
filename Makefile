VERSION ?= 0.1.0
IMAGE ?= argus-os:$(VERSION)
AURORABOOT ?= quay.io/kairos/auroraboot:v0.28.0@sha256:9a3ae40ca2f4e5856055502ad72ff3b6343758b78a97968387ee3af0233892c2

.PHONY: app manager hostd os iso

app:
	docker buildx build --load -t argus:$(VERSION) .
	mkdir -p os/images
	docker save -o os/images/argus.tar argus:$(VERSION)

manager:
	docker buildx build --load --build-arg CMD=argus-manager -t argus-manager:$(VERSION) .

hostd:
	docker buildx build --build-arg CMD=argus-hostd --target bin --output type=local,dest=os/build .
	mv os/build/app os/build/argus-hostd

os: app hostd
	docker buildx build --load --build-arg VERSION=$(VERSION) -t $(IMAGE) os

iso: os
	docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v $(CURDIR)/build:/output \
		$(AURORABOOT) build-iso --output /output/ oci:$(IMAGE)
