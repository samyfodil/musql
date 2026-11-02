.PHONY: all build_all_targets clean test harness vet bench-pointread cloud cloud-console cloud-run

all: vet test

# Nothing here is platform-specific (no CGo), so a
# plain cross-compile of every supported target is the whole matrix check.
build_all_targets:
	for pair in darwin/amd64 darwin/arm64 freebsd/amd64 linux/386 linux/amd64 linux/arm linux/arm64 \
		linux/loong64 linux/ppc64le linux/riscv64 linux/s390x netbsd/amd64 openbsd/amd64 \
		openbsd/arm64 windows/386 windows/amd64 windows/arm64; do \
		GOOS=$${pair%/*} GOARCH=$${pair#*/} go build -o /dev/null ./... || exit 1; \
	done
	@echo done

vet:
	go vet ./...
	cd hrana && go vet ./...
	cd examples/libp2p && go vet ./...
	cd bench/pointread && go vet -tags musql ./... && CGO_ENABLED=1 go vet -tags mattn .

test:
	go test ./...
	cd hrana && go test ./...
	cd examples/libp2p && go test ./...
	@$(MAKE) --no-print-directory check-no-stray-dbs

# Tests must never leave a database behind in the source tree: every one of
# them belongs under t.TempDir(), and an in-memory DSN is backed beneath
# os.TempDir() (puredriver/memdb.go). This regressed once -- a bare
# "file:n1?mode=memory" DSN was opened as a RELATIVE PATH, littering replication/
# with files named n1, gate, syncA and even ":memory:", which then got
# committed by accident -- so the invariant is enforced rather than assumed.
# driver's TestMemoryDSNsCreateNoFilesInCWD pins the root-cause class;
# this catches anything that writes to the tree by any other route.
#
# BOTH FORMATS ARE MATCHED. The magic check was 'SQLite format 3' alone, which
# stopped catching anything the moment tests started writing the NATIVE format --
# a stray .musq (or its -delta / .lock sidecar) is exactly the same mistake and was
# invisible to this gate. The extensionless case is what the magic check is for:
# the historical strays had no suffix at all.
check-no-stray-dbs:
	@found=$$(find . -path ./.git -prune -o -type f \
	    \( -name '*.db' -o -name '*.db-journal' -o -name '*.db-wal' -o -name '*.db-shm' \
	       -o -name '*.sqlite' -o -name '*.sqlite-journal' -o -name ':memory:' \
	       -o -name '*.musq' -o -name '*.musq-delta' -o -name '*.musq.lock' \) -print \
	    | grep -v '/testdata/' || true); \
	  magic=$$(git status --porcelain --untracked-files=all \
	    | sed -n 's/^?? //p' | grep -v '/testdata/' \
	    | while read -r f; do \
	        [ -f "$$f" ] || continue; \
	        case "$$(head -c 15 "$$f" 2>/dev/null)" in \
	          'SQLite format 3'*) echo "$$f" ;; \
	          MQSF*) echo "$$f" ;; \
	        esac; \
	      done || true); \
	  all=$$(printf '%s\n%s' "$$found" "$$magic" | sed '/^$$/d' | sort -u); \
	  if [ -n "$$all" ]; then \
	    echo 'stray database files left in the source tree:'; \
	    echo "$$all" | sed 's/^/  /'; \
	    echo 'tests must write databases under t.TempDir() (see driver/memdb.go).'; \
	    exit 1; \
	  fi

# Differential tests against real C SQLite (CGo, separate nested module), whose
# oracle needs the SQL math functions.
harness:
	cd compat-harness && go test -tags sqlite_math_functions ./...

# Bulk load + point reads vs C SQLite (bench/pointread, its own CGo module). The
# short run; POINTREAD_FULL=1 for 10M rows. Fails if musql loses a gated metric.
bench-pointread:
	bench/pointread/run

# musql cloud: the server, the CLI and the console, into bin/. TAGS=jitllm
# builds the server with embedded AI.
cloud: cloud-console
	cd cloud && go build -tags '$(TAGS)' -o ../bin/musql-cloud ./cmd/musql-cloud
	cd cloud && go build -o ../bin/musql ./cmd/musql

cloud-console:
	sh npm/build.sh
	cd cloud/console && npm ci && npm run build

# A standalone node on :8080 with its data in .cloud-dev/. The first run
# copies cloud/config.example.yaml and asks for the admin's password
# (ADMIN_EMAIL, default admin@example.com).
ADMIN_EMAIL ?= admin@example.com
cloud-run: cloud
	mkdir -p .cloud-dev
	test -f .cloud-dev/config.yaml || cp cloud/config.example.yaml .cloud-dev/config.yaml
	test -d .cloud-dev/data || bin/musql-cloud init -config .cloud-dev/config.yaml --admin-email $(ADMIN_EMAIL)
	bin/musql-cloud -config .cloud-dev/config.yaml

clean:
	rm -f log-* cpu.test mem.test *.out go.work*
	go clean
