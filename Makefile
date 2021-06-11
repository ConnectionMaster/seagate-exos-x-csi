ifndef DOCKER_HUB_REPOSITORY
	DOCKER_HUB_REPOSITORY = docker.io/seagatecsi
endif

ifndef VERSION
	VERSION = latest
else
	VERSION_FLAG = -X github.com/Seagate/seagate-exos-x-csi/pkg/common.Version=$(VERSION)
endif

ifndef BIN
	BIN = seagate-csi
endif

IMAGE = $(DOCKER_HUB_REPOSITORY)/$(BIN):$(VERSION)

help:
	@echo ""
	@echo "Build Targets:"
	@echo "-----------------------------------------------------------------------------------"
	@echo "make clean      - remove '$(BIN)-controller' and '$(BIN)-node'"
	@echo "make all        - create controller and node driver images, create docker image"
	@echo "make bin        - create controller and node driver images"
	@echo "make controller - create controller driver image ($(BIN)-controller)"
	@echo "make node       - create node driver image ($(BIN)-node)"
	@echo "make test       - build test/sanity"
	@echo "make image      - create a repo docker image ($(IMAGE))"
	@echo "make limage     - create a local docker image ($(IMAGE))"
	@echo "make push       - push the docker image to '$(DOCKER_HUB_REPOSITORY)'"
	@echo ""

all:		bin rimage
.PHONY: all

bin: controller node
.PHONY: bin

controller:
	go build -v -ldflags "$(VERSION_FLAG)" -o $(BIN)-controller ./cmd/controller
.PHONY: controller

node:
	echo "$(VERSION_FLAG)"
	go build -v -ldflags "$(VERSION_FLAG)" -o $(BIN)-node ./cmd/node
.PHONY: node

test:
	./test/sanity
.PHONY: test

image:
	docker build -t $(IMAGE) --build-arg version="$(VERSION)" --build-arg vcs_ref="$(shell git rev-parse HEAD)" --build-arg build_date="$(shell date --rfc-3339=seconds)" .
.PHONY: image

limage:
	docker build -f Dockerfile.local -t $(IMAGE) --build-arg version="$(VERSION)" --build-arg vcs_ref="$(shell git rev-parse HEAD)" --build-arg build_date="$(shell date --rfc-3339=seconds)" .
.PHONY: limage

push:
	docker push $(IMAGE)
.PHONY: push

clean:
	rm -vf $(BIN)-controller $(BIN)-node
.PHONY: clean
