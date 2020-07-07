.PHONY: default help build package tag push helm helm-migration run test clean

SHELL         = /bin/bash
APP_NAME      = pg2kafka
VERSION      := $(shell git describe --always --tags)
GIT_COMMIT    = $(shell git rev-parse HEAD)
GIT_DIRTY     = $(shell test -n "`git status --porcelain`" && echo "+CHANGES" || true)
BUILD_DATE    = $(shell date '+%Y-%m-%d-%H:%M:%S')
SQUAD         = "backend"

default: help

help:
	@echo 'Management commands for ${APP_NAME}:'
	@echo
	@echo 'Usage:'
	@echo '    make build                 Compile the project.'
	@echo '    make package               Build final Docker image with just the Go binary inside.'
	@echo '    make tag                   Tag image created by package with latest, git commit and version.'
	@echo '    make push                  Push tagged images to registry.'
	@echo '    make helm                  Deploy to Kubernetes via Helm.'
	@echo '    make helm-migration        Run database migration via Helm.'
	@echo '    make run ARGS=             Run with supplied arguments.'
	@echo '    make test                  Run tests on a compiled project.'
	@echo '    make clean                 Clean the directory tree.'

	@echo

build:
	@echo "Building ${APP_NAME} ${VERSION}"
	go build -ldflags "-w -X github.com/kitabisa/pg2kafka/version.GitCommit=${GIT_COMMIT}${GIT_DIRTY} -X github.com/kitabisa/pg2kafka/version.Version=${VERSION} -X github.com/kitabisa/pg2kafka/version.Environment=${ENVIRONMENT} -X github.com/kitabisa/pg2kafka/version.BuildDate=${BUILD_DATE}" -o bin/${APP_NAME}

package:
	@echo "Building image ${APP_NAME} ${VERSION} ${GIT_COMMIT}"
	docker build --build-arg VERSION=${VERSION} --build-arg GIT_COMMIT=${GIT_COMMIT}${GIT_DIRTY} -t ${REGISTRY_URL}/${APP_NAME}:local .

tag: package
	@echo "Tagging: latest ${VERSION} ${GIT_COMMIT}"
	docker tag ${REGISTRY_URL}/${APP_NAME}:local ${REGISTRY_URL}/${APP_NAME}:${GIT_COMMIT}
	docker tag ${REGISTRY_URL}/${APP_NAME}:local ${REGISTRY_URL}/${APP_NAME}:${VERSION}
	docker tag ${REGISTRY_URL}/${APP_NAME}:local ${REGISTRY_URL}/${APP_NAME}:latest

push: tag
	@echo "Pushing Docker image to registry: latest ${VERSION} ${GIT_COMMIT}"
	docker push ${REGISTRY_URL}/${APP_NAME}:${GIT_COMMIT}
	docker push ${REGISTRY_URL}/${APP_NAME}:${VERSION}
	docker push ${REGISTRY_URL}/${APP_NAME}:latest

helm:
	@echo "Deploying ${APP_NAME}-urunan ${VERSION}"
	helm upgrade ${APP_NAME}-urunan kitabisa/app --install \
		--version 0.12.0-alpha.10 \
		--namespace urunan \
		--values _infra/k8s/urunan/${ENVIRONMENT}.yaml \
		--set meta.env=${ENVIRONMENT},meta.squad=${SQUAD},meta.version=${VERSION},image.repository=${REGISTRY_URL}/${APP_NAME},image.tag=${VERSION}

helm-migration:
	@echo "Migrating database ${APP_NAME} ${VERSION}"
	helm upgrade ${APP_NAME}-migration kitabisa/migration --install --cleanup-on-fail \
		--version 0.2.0-alpha.3 \
		--namespace ${APP_NAME} \
		--values _infra/k8s/${ENVIRONMENT}.yaml \
		--set meta.env=${ENVIRONMENT},meta.squad=${SQUAD},meta.version=${VERSION},image.repository=${REGISTRY_URL}/${APP_NAME},image.tag=${VERSION}

run: build
	@echo "Running ${APP_NAME} ${VERSION}"
	bin/${APP_NAME} ${ARGS}

test:
	@echo "Testing ${APP_NAME} ${VERSION}"
	go test ./...

clean:
	@echo "Removing ${APP_NAME} ${VERSION}"
	@test ! -e bin/${APP_NAME} || rm bin/${APP_NAME}
