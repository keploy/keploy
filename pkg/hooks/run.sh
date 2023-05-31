#! /bin/sh

export BPF_CLANG=clang-14
export BPF_CFLAGS="-I/usr/include/x86_64-linux-gnu -D__x86_64__ -O2 -g -Wall -Werror"
export TARGET=amd64
export KEPLOY_MODE=record
export HOST=localhost
export PORT=8081
export KEPLOY_TEST_PATH=/home/ubuntu/ebpf/keploy-ebpf-poc/Keploy-Tests/tests
export KEPLOY_MOCK_PATH=/home/ubuntu/ebpf/keploy-ebpf-poc/Keploy-Tests/mocks
# To compile and run the ebpf prog”ram...
go generate ./... && go run -exec "sudo -E" ./
