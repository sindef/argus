VERSION ?= 0.1.0
ARCH ?= amd64
TAG ?= $(VERSION)$(if $(filter-out amd64,$(ARCH)),-$(ARCH))
IMAGE ?= argus-os:$(TAG)
PLATFORM = --platform linux/$(ARCH)
SOURCES = build/third-party-sources-$(ARCH)
BUNDLE = build/argus-third-party-sources-$(VERSION)-$(ARCH).tar.gz
AURORABOOT ?= quay.io/kairos/auroraboot:v0.28.0@sha256:9a3ae40ca2f4e5856055502ad72ff3b6343758b78a97968387ee3af0233892c2

.PHONY: app manager hostd os iso notices notices-check source-bundle

notices:
	sh scripts/update-notices.sh

notices-check:
	sh scripts/update-notices.sh --check

app:
	docker buildx build $(PLATFORM) --load -t argus:$(VERSION) .
	mkdir -p os/images
	docker save -o os/images/argus.tar argus:$(VERSION)

manager:
	docker buildx build $(PLATFORM) --load --build-arg CMD=argus-manager -t argus-manager:$(TAG) .

hostd:
	docker buildx build $(PLATFORM) --build-arg CMD=argus-hostd --target bin --output type=local,dest=os/build .
	mv os/build/app os/build/argus-hostd

os: app hostd
	docker buildx build $(PLATFORM) --load --build-arg VERSION=$(VERSION) -t $(IMAGE) os

iso: os
	docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v $(CURDIR)/build:/output \
		$(AURORABOOT) build-iso --arch $(ARCH) --output /output/ oci:$(IMAGE)

source-bundle: os
	rm -rf $(SOURCES)
	docker buildx build $(PLATFORM) --build-arg VERSION=$(VERSION) --target source-bundle --output type=local,dest=$(SOURCES) os
	docker buildx build $(PLATFORM) --target source-bundle --output type=local,dest=$(SOURCES) .
	cd $(SOURCES) && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS
	rm -f $(BUNDLE)*
	tar -czf $(BUNDLE) -C $(SOURCES) .
	if [ "$$(stat -c %s $(BUNDLE))" -ge 1900000000 ]; then \
		split -b 1900000000 $(BUNDLE) $(BUNDLE).part-; \
		rm $(BUNDLE); \
	fi
