# Checks and release builds of hs2; run in hs2-src/ (R1 merge prep).
#
# The engine package alone runs ~12-13 minutes (733 s in the R1 round-3
# judge's run), past go test's default of 10 minutes a package, so a bare
# `go test ./...` cuts it; the suite needs its own timeout (TEST_TIMEOUT,
# per package). smux is a module of its own (third_party/smux), outside ./...
#
#   make check     vet, gofmt and the full suite of both modules
#   make test      go test ./... (hs2's module)
#   make smux      go test ./... in third_party/smux
#   make race      the race detector: engine's R1 cores (RACE_RUN; the
#                  emulated-link timing tests stay out: -race slows them
#                  past their bounds), tlscarrier, cmd/..., and smux's
#                  scheduler, groups and coalescing
#   make test386   the packages R1 changed, GOARCH=386, -short: 64-bit
#                  atomics on a 32-bit platform (REDESIGN §5.10 step 5)
#   make release   static binaries (CGO_ENABLED=0, -trimpath, -s -w: as
#                  BUILD.md's release build) for linux amd64, 386 and arm64
#                  in DIST, and their SHA256SUMS

GO           ?= go
GOFMT        ?= gofmt
TEST_TIMEOUT ?= 25m
RACE_TIMEOUT ?= 30m
RACE_RUN     ?= Hint|MoveBack|MoveRule|QuietCounts|QuietMove|QuietTunPlacement|OWD|Budget|Panel|LinkSndbuf|Moved|UDPLate|LocalTarget|Group|Coalesce|Events|QuietRecords|HostQ|Lowat|Gate|HostWait
SMUX_RACE    ?= Group|Coalesce|Sched|Urgent
R1_PKGS      ?= ./engine ./tlscarrier ./cmd/hs2
DIST         ?= dist
ARCHES       ?= amd64 386 arm64

.PHONY: check vet fmt test smux race test386 release

check: vet fmt test smux

vet:
	$(GO) vet ./...
	cd third_party/smux && $(GO) vet ./...

fmt:
	@out=$$($(GOFMT) -l .); if [ -n "$$out" ]; then echo "gofmt -l:"; echo "$$out"; exit 1; fi

test:
	$(GO) test -count=1 -timeout $(TEST_TIMEOUT) ./...

smux:
	cd third_party/smux && $(GO) test -count=1 -timeout $(TEST_TIMEOUT) ./...

race:
	$(GO) test -race -count=1 -timeout $(RACE_TIMEOUT) -run '$(RACE_RUN)' ./engine
	$(GO) test -race -count=1 -timeout $(RACE_TIMEOUT) ./tlscarrier ./cmd/...
	cd third_party/smux && $(GO) test -race -count=1 -timeout $(RACE_TIMEOUT) -run '$(SMUX_RACE)' ./...

test386:
	GOARCH=386 $(GO) test -short -count=1 -timeout $(TEST_TIMEOUT) $(R1_PKGS)

release:
	mkdir -p $(DIST)
	for a in $(ARCHES); do CGO_ENABLED=0 GOOS=linux GOARCH=$$a $(GO) build -trimpath -ldflags="-s -w" -o $(DIST)/hs2-linux-$$a ./cmd/hs2 || exit 1; done
	cd $(DIST) && sha256sum hs2-linux-* > SHA256SUMS && cat SHA256SUMS
