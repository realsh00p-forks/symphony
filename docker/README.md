# Building Symphony API for aarch64

This image builds the Rust provider binding and Go API binary for Linux aarch64
using a Debian 11 Bullseye cross-toolchain. Bullseye is used so the generated
artifacts run on targets with glibc 2.31, such as the `linaro` target.

Build the builder image from the repository root:

```bash
docker build -f docker/Dockerfile -t symphony-aarch64-builder .
```

Build the Rust provider binding and Go binary:

```bash
docker run --rm -v "$PWD":/work -w /work symphony-aarch64-builder bash -lc '
set -euo pipefail

cd api/pkg/apis/v1alpha1/providers/target/rust
cargo build --release --target aarch64-unknown-linux-gnu

cd /work/api
export LIBDIR=/work/api/pkg/apis/v1alpha1/providers/target/rust/target/aarch64-unknown-linux-gnu/release
CGO_ENABLED=1 \
GOARCH=arm64 \
GOOS=linux \
CC=aarch64-linux-gnu-gcc \
CGO_LDFLAGS="-L$LIBDIR" \
go build -buildvcs=false -o symphony-api-linux-arm64-glibc231
'
```

Artifacts:

```text
api/symphony-api-linux-arm64-glibc231
api/pkg/apis/v1alpha1/providers/target/rust/target/aarch64-unknown-linux-gnu/release/libsymphony.so
```

Verify the output architecture and glibc symbol requirements:

```bash
file api/symphony-api-linux-arm64-glibc231
file api/pkg/apis/v1alpha1/providers/target/rust/target/aarch64-unknown-linux-gnu/release/libsymphony.so

strings api/symphony-api-linux-arm64-glibc231 | grep -o 'GLIBC_[0-9.]*' | sort -Vu | tail -1
strings api/pkg/apis/v1alpha1/providers/target/rust/target/aarch64-unknown-linux-gnu/release/libsymphony.so | grep -o 'GLIBC_[0-9.]*' | sort -Vu | tail -1
```

On the aarch64 target, install the rebuilt library and run the binary:

```bash
sudo cp libsymphony.so /usr/local/lib/
sudo ldconfig
./symphony-api-linux-arm64-glibc231 --help
```
