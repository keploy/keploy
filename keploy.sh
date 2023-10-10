#!/bin/bash

IS_CI=false

for arg in "$@"
do
    case $arg in
        -isCI)
            IS_CI=true
            shift
        ;;
        *)
        ;;
    esac
done

install_keploy_arm() {
    curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz" | tar xz -C /tmp

    sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin

    keploy
}

install_keploy_amd() {
    curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz" | tar xz -C /tmp

    sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin

    keploy
}

install_colima_docker() {
    if ! docker network ls | grep -q 'keploy-network'; then
        docker network create keploy-network
    fi
    alias keploy='docker run --name keploy-v2 -p 16789:16789 --network keploy-network --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm ghcr.io/keploy/keploy'
}

install_docker() {
    if ! docker network ls | grep -q 'keploy-network'; then
        docker network create keploy-network
    fi
    alias keploy='sudo docker run --name keploy-v2 -p 16789:16789 --network keploy-network --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm ghcr.io/keploy/keploy'
}

ARCH=$(uname -m)

if [ "$IS_CI" = false ]; then
    OS_NAME="$(uname -s)"
    if [ "$OS_NAME" = "Darwin" ]; then
        if command -v brew &> /dev/null
        then
            if ! brew list colima &> /dev/null; then
                brew install colima
            fi
            if colima status | grep -q "Running"; then
                echo "colima is already running."
            else
                colima start
            fi
            install_colima_docker
        else
            echo "brew is not installed, install brew for easy installation"
        fi
    elif [ "$OS_NAME" = "Linux" ]; then
        echo -n "Do you want to install keploy with Linux or Docker? (linux/docker): "
        read user_input
        if [ "$user_input" = "linux" ]; then
            if [ "$ARCH" = "x86_64" ]; then
                install_keploy_amd
            elif [ "$ARCH" = "aarch64" ]; then
                install_keploy_arm
            else
                echo "Unsupported architecture: $ARCH"
                exit 1
            fi
        else
            install_docker
        fi
    elif [[ "$OS_NAME" == MINGW32_NT* ]]; then
        echo "Windows not supported please run on WSL2"
    elif [[ "$OS_NAME" == MINGW64_NT* ]]; then
        echo "Windows not supported please run on WSL2"
    else
        echo "Unknown OS, install Linux to run Keploy"
    fi
else
    if [ "$ARCH" = "x86_64" ]; then
        install_keploy_amd
    elif [ "$ARCH" = "aarch64" ]; then
        install_keploy_arm
    else
        echo "Unsupported architecture: $ARCH"
        exit 1
    fi
fi