BINARY  := cex-dra-kubeletplugin
PKG     := ./cmd/cex-dra-kubeletplugin
GOOS    ?= linux
GOARCH  ?= s390x
PREFIX  ?= /usr/local

# Build identity stamped into the binary (--version, startup log line).
# --long keeps the commit id in the string on a commit that is exactly a tag
# (v1.0.0-alpha.0-0-gb62b637 instead of v1.0.0-alpha.0), so a running build
# names the commit it was built from even when the tag has since moved.
# Falls back to "dev" where git or .git is unavailable (image build
# context, nix sandbox). The image build passes VERSION through instead.
VERSION ?= $(shell git describe --tags --always --dirty --long 2>/dev/null || echo dev)
LDFLAGS := -s -w -X k8s-cex-dra-driver/internal/version.version=$(VERSION)

GO_PKGS       := ./cmd/... ./internal/...
GO_DIRS       := cmd internal
HEALTH_DIR    := .health
HEALTH_REPORT := $(HEALTH_DIR)/report.json

# Bash gate: this tree's own scripts, named one by one rather than
# swept, so a sweep cannot pull in vendored or third-party material.
# These two are user-facing: the installation docs tell a reader to run
# them. An optional include may add to the list.
SH_SRC := deploy/k8s-deploy-dev-overlay.sh deploy/k8s-undeploy-dev-overlay.sh

# The gate set, as a variable rather than a literal prerequisite list, so
# an optional include can extend it. Every target named here runs with
# nothing but this tree.
CHECK_TARGETS := fmt-check lint vulncheck test manifests-check \
                 bash-check md-fmt-check

.PHONY: all install clean \
        fmt fmt-check lint vulncheck test manifests-check bash-check \
        md-fmt md-fmt-check docs \
        check health ci-health-report

# Both includes are optional and keyed on the tree rather than on the
# environment: a directory that is not here takes its rules with it, and
# this file still parses on its own, which is what lets the image build
# context carry nothing else. `make docs` answers either way. Set the
# default goal explicitly, or the include order would decide it.
#
# Both includes come after the variables they extend.
.DEFAULT_GOAL := all
ifneq ($(wildcard dev/dev.mk),)
include dev/dev.mk
endif
ifneq ($(wildcard site/site.mk),)
include site/site.mk
else
docs:
	@echo "docs: no site renderer in this tree; the doc set is docs/"
endif

all:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

install: all
	install -D -m 0755 $(BINARY) $(PREFIX)/bin/$(BINARY)

clean:
	rm -f $(BINARY)
	rm -rf $(HEALTH_DIR)

fmt:
	gofmt -w $(GO_DIRS)
	goimports -w $(GO_DIRS)

fmt-check:
	@unformatted=$$(gofmt -l $(GO_DIRS); goimports -l $(GO_DIRS)); \
	if [ -n "$$unformatted" ]; then \
		echo "Unformatted files:"; echo "$$unformatted"; exit 1; \
	fi

lint:
	golangci-lint run $(GO_PKGS)

vulncheck:
	govulncheck $(GO_PKGS)

test:
	go test -race -cover $(GO_PKGS)

manifests-check:
	kustomize build deploy/kustomize/base > /dev/null
	kustomize build deploy/kustomize/overlays/template > /dev/null
	kustomize build deploy/kustomize/overlays/dev-unpriv > /dev/null
	kustomize build deploy/kustomize/overlays/dev-priv > /dev/null
	kustomize build deploy/kustomize/overlays/dev-alpha > /dev/null
	@for d in deploy/kustomize/components/feature-*; do \
		[ -d "$$d" ] || continue; \
		grep -q "^  - \.\./\.\./components/$${d##*/}\$$" \
			deploy/kustomize/overlays/dev-alpha/kustomization.yaml || \
			{ echo "dev-alpha: missing component $${d##*/}"; exit 1; }; \
	done
	@kustomize build deploy/kustomize/overlays/dev-alpha \
		| grep -A1 '^ *- name: FEATURE_GATES$$' \
		| grep -q '^ *value: AllAlpha=true$$' || \
		{ echo "dev-alpha: FEATURE_GATES must render as exactly AllAlpha=true"; exit 1; }
	@# The health port and the probe that dials it are one unit: the binary
	@# disables the service on a negative port, and Kustomize cannot drop a
	@# probe conditionally, so the two must never be edited apart. The port
	@# travels as env, because an args patch replaces the whole list.
	@port=$$(kustomize build deploy/kustomize/base \
		| grep -A1 '^ *- name: HEALTHCHECK_PORT$$' \
		| sed -n 's/^ *value: "\(.*\)"$$/\1/p'); \
	probe=$$(kustomize build deploy/kustomize/base \
		| sed -n '/^ *livenessProbe:/,/^ *name:/p' \
		| sed -n 's/^ *port: \(.*\)$$/\1/p'); \
	[ -n "$$port" ] || { echo "base: HEALTHCHECK_PORT must be set as an env entry"; exit 1; }; \
	[ "$$port" = "$$probe" ] || \
		{ echo "base: livenessProbe port ($$probe) must match HEALTHCHECK_PORT ($$port)"; exit 1; }
	@kustomize build deploy/kustomize/base \
		| sed -n '/^ *livenessProbe:/,/^ *name:/p' \
		| grep -q '^ *service: liveness$$' || \
		{ echo "base: livenessProbe must name the liveness service"; exit 1; }
	@kustomize build deploy/kustomize/overlays/template \
		| grep -q '^ *- name: HEALTHCHECK_PORT$$' || \
		{ echo "template: HEALTHCHECK_PORT must survive the overlay"; exit 1; }

# The KubeVirt CI deploy tree, in its own block so the hunk stays
# independent of the list above when it is picked onto a release branch.
SH_SRC += contrib/kubevirt-ci/cex-dra.sh \
          contrib/kubevirt-ci/lib/assert.sh contrib/kubevirt-ci/lib/driver.sh \
          contrib/kubevirt-ci/lib/helpers.sh contrib/kubevirt-ci/lib/uninstall.sh \
          contrib/kubevirt-ci/lib/workloads.sh

bash-check:
	@if [ -n "$(SH_SRC)" ]; then \
		bash -n $(SH_SRC); \
		shellcheck -e SC1091 $(SH_SRC); \
		shfmt -i 2 -ci -d $(SH_SRC); \
	fi

# The doc set's prose (.prettierrc.yaml). The sentence-per-line
# convention itself is not machine-checked. Prettier normalizes
# structure (lists, tables, spacing) and preserves line breaks. The
# scope is all of docs/, which holds the doc set and nothing else, so
# the glob needs no exceptions.
MD_DOCS := "docs/**/*.md"

md-fmt:
	prettier --write $(MD_DOCS)

md-fmt-check:
	prettier --check $(MD_DOCS)

check: $(CHECK_TARGETS)

health:
	golangci-lint run --config .golangci-health.yml --issues-exit-code=0 $(GO_PKGS)

ci-health-report:
	mkdir -p $(HEALTH_DIR)
	golangci-lint run --config .golangci-health.yml --issues-exit-code=0 \
		--output.text.path=stdout --output.json.path=$(HEALTH_REPORT) $(GO_PKGS)
