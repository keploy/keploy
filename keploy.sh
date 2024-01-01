#!/bin/bash

# Function to display the menu and handle user selection
platform="Linux"
OS_NAME="$(uname -s)"
IS_CI=false

# Check for CI environment
for arg in "$@"; do
    case $arg in
        -isCI)
            IS_CI=true
            shift
        ;;
        *)
        ;;
    esac
done

# Function to check and install fzf
checkForfzf() {
    if [[ $OS_NAME = "Linux" ]]; then
        if ! command -v fzf &> /dev/null; then
            echo "fzf is not installed. Installing fzf..."
            sudo apt-get install fzf
        fi
    elif [[ $OS_NAME = "Darwin" ]]; then
        if ! command -v fzf &> /dev/null; then
            echo "fzf is not installed. Installing fzf..."
            brew install fzf
        fi
    fi
}

# Function to display installation options menu
displaymenu() {
    echo "Do you want to install Keploy in Linux or Docker?"

    local options=("Linux" "Docker")
    local choice=$(printf "%s\n" "${options[@]}" | fzf --height 20% --border --prompt 'Select installation method: ')

    case "$choice" in
        "Linux")
            platform="Linux"
            ;;
        "Docker")
            platform="Docker"
            ;;
        *)
            echo "Invalid option selected."
            return 1
            ;;
    esac
}

# Function to install Keploy for ARM architecture
install_keploy_arm() {
    curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz" | tar xz -C /tmp
    sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin/keploybin
}

# Function to install Keploy for AMD architecture
install_keploy_amd() {
    curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz" | tar xz -C /tmp
    sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin/keploybin
}

# Function to install Keploy on Linux
installOnLinux() {
    local ARCH=$(uname -m)
    case "$ARCH" in
        "x86_64")
            install_keploy_amd
            ;;
        "aarch64")
            install_keploy_arm
            ;;
        *)
            echo "Unsupported architecture: $ARCH"
            return 1
            ;;
    esac
}

# Function to install Keploy on Docker
installOnDocker() {
    # Check if Docker is installed
    if ! command -v docker &> /dev/null; then
        echo "Docker is not installed. Please install Docker first."
        return 1
    fi

    # Create Keploy network if it doesn't exist
    if ! docker network ls | grep -q 'keploy-network'; then
        echo "Creating Keploy network..."
        docker network create keploy-network
    fi

    # Set up the Keploy Docker alias depending on the OS
    if [ "$OS_NAME" = "Linux" ]; then
        alias keploy='sudo docker run --pull always --name keploy-v2 -p 16789:16789 --network keploy-network --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm ghcr.io/keploy/keploy'
    elif [ "$OS_NAME" = "Darwin" ]; then
        alias keploy='sudo docker run --pull always --name keploy-v2 -p 16789:16789 --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v '"$HOME"'/.keploy-config:/root/.keploy-config -v '"$HOME"'/.keploy:/root/.keploy --rm ghcr.io/keploy/keploy'
    fi

    echo "Keploy Docker setup is complete. You can now use 'keploy' command."
}

# Main function
installKeploy() {
    if [ "$OS_NAME" = "Darwin" ]; then
        installOnDocker
    elif [ "$IS_CI" = false ]; then
        checkForfzf
        displaymenu
        
        case "$platform" in
            "Linux")
                installOnLinux
                ;;
            "Docker")
                installOnDocker
                ;;
        esac
    else
        echo "Running in CI environment. Skipping interactive menu."
    fi
}

installKeploy

if command -v keploy &> /dev/null; then
    keploy example
    rm keploy.sh
fi
