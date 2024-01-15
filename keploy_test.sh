#!/bin/bash

installKeploy() {
    IS_CI=false
    CACHE_FILE="$HOME/.keploy_cache"

    # Declare variables to store different input values
    DOCKER_INPUT=""
    COLIMA_INPUT=""
    LINUX_INPUT=""

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

    # Read user input from the cache file
    if [ -f "$CACHE_FILE" ]; then
        source "$CACHE_FILE"
    fi

    # Function to prompt and store user input in cache
    prompt_and_store_input() {
        local input_var_name=$1
        local prompt_message=$2

        # Check if the variable is already set in the cache
        if [ -z "${!input_var_name}" ]; then
            echo -n "$prompt_message"
            read user_input
            eval "$input_var_name=$user_input"
            echo "$input_var_name=\"$user_input\"" >>"$CACHE_FILE"
        fi
    }


    get_current_docker_context() {
        current_context=$(docker context ls --format '{{.Name}} {{if .Current}}*{{end}}' | grep '*' | awk '{print $1}')
    }

    install_keploy_arm() {
        curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz" | tar xz -C /tmp

        sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin/keploybin

        set_alias
    }

    install_keploy_amd() {
        curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz" | tar xz -C /tmp

        sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin/keploybin

        set_alias
    }

    set_alias() {
        ALIAS_CMD="alias keploy='sudo -E env PATH=\"\$PATH\" keploybin'"
        current_shell=$(ps -p $$ -ocomm=)
        if [ "$current_shell" = "zsh" ]; then
            if [ -f ~/.zshrc ]; then
                if grep -q "alias keploy=" ~/.zshrc; then
                    sed -i '/alias keploy/d' ~/.zshrc
                fi
                echo "$ALIAS_CMD" >> ~/.zshrc
                source ~/.zshrc
            else
                alias keploy='sudo -E env PATH="$PATH" keploybin'
            fi
        elif [ "$current_shell" = "bash" ]; then
            if [ -f ~/.bashrc ]; then
                if grep -q "alias keploy=" ~/.bashrc; then
                    sed -i '/alias keploy/d' ~/.bashrc
                fi
                echo "$ALIAS_CMD" >> ~/.bashrc
                source ~/.bashrc
            else
                alias keploy='sudo -E env PATH="$PATH" keploybin'
            fi
        else
            alias keploy='sudo -E env PATH="$PATH" keploybin'
        fi
    }


    install_colima_docker() {
        if ! docker network ls | grep -q 'keploy-network'; then
            docker network create keploy-network
        fi
        alias keploy='docker run --pull always --name keploy-v2 -p 16789:16789 --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v '"$HOME"'/.keploy-config:/root/.keploy-config -v '"$HOME"'/.keploy:/root/.keploy --rm ghcr.io/keploy/keploy'
    }

    install_docker() {
        if ! docker network ls | grep -q 'keploy-network'; then
            docker network create keploy-network
        fi

        if [ "$OS_NAME" = "Darwin" ]; then
            if ! docker volume inspect debugfs &>/dev/null; then
                docker volume create --driver local --opt type=debugfs --opt device=debugfs debugfs
            fi
            alias keploy='sudo docker run --pull always --name keploy-v2 -p 16789:16789 --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v '"$HOME"'/.keploy-config:/root/.keploy-config -v '"$HOME"'/.keploy:/root/.keploy --rm ghcr.io/keploy/keploy'
        else
            alias keploy='sudo docker run --pull always --name keploy-v2 -p 16789:16789 --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v '"$HOME"'/.keploy-config:/root/.keploy-config -v '"$HOME"'/.keploy:/root/.keploy --rm ghcr.io/keploy/keploy'
        fi

    }


    ARCH=$(uname -m)

    if [ "$IS_CI" = false ]; then
        OS_NAME="$(uname -s)"
        if [ "$OS_NAME" = "Darwin" ]; then
            get_current_docker_context
            if ! which docker &>/dev/null; then
                prompt_and_store_input "DOCKER_INPUT" "Docker not found on device, install docker? (y/n): "
                if [ "$DOCKER_INPUT" = "y" ]; then
                    echo "Installing docker via brew"
                    if command -v brew &>/dev/null; then
                        brew install docker
                    else
                        echo "\e]8;;https://brew.sh\abrew is not installed, install brew for easy docker installation\e]8;;\a"
                        return
                    fi
                elif [ "$DOCKER_INPUT" != "n" ]; then
                    echo "Please enter a valid command"
                    return
                else
                    echo "Please install docker to install keploy"
                    return
                fi
            fi

            prompt_and_store_input "COLIMA_INPUT" "Do you want to install keploy with Docker or Colima? (docker/colima): "

            if [ "$COLIMA_INPUT" = "colima" ]; then
                if [ "$current_context" = "default" ]; then
                    echo 'Error: Docker is using the default context, set to colima using "docker context use colima"'
                    return
                fi
                if ! which colima &>/dev/null; then
                    echo -e "\e]8;;https://kumojin.com/en/colima-alternative-docker-desktop\aAlternate is to use colima (lightweight and performant alternative to Docker Desktop)\e]8;;\a"
                    prompt_and_store_input "COLIMA_INSTALL" "Install colima (y/n): "
                    if [ "$COLIMA_INSTALL" = "y" ]; then
                        echo "Installing colima via brew"
                        if command -v brew &>/dev/null; then
                            brew install colima
                        else
                            echo "\e]8;;https://brew.sh\abrew is not installed, install brew for easy colima installation\e]8;;\a"
                            return
                        fi
                    elif [ "$COLIMA_INSTALL" = "n" ]; then
                        echo "Please install Colima to install Keploy."
                        return
                    else
                        echo "Please enter a valid command"
                        return
                    fi
                else
                    prompt_and_store_input "COLIMA_PROCEED" "colima found on your system, would you like to proceed with it? (y/n): "
                    if [ "$COLIMA_PROCEED" = "n" ]; then
                        echo "Please allow Colima to run Keploy."
                        return
                    elif [ "$COLIMA_PROCEED" != "y" ]; then
                        echo "Please enter a valid command"
                        return
                    fi
                fi

                if colima status | grep -q "Running"; then
                    echo "colima is already running."
                else
                    colima start
                fi
                install_colima_docker

            elif [ "$COLIMA_INPUT" = "docker" ]; then
                if [ "$current_context" = "colima" ]; then
                    echo 'Error: Docker is using the colima context, set to default using "docker context use default"'
                    return
                fi
                install_docker

            else
                echo "Please enter a valid command"
            fi
            return

        elif [ "$OS_NAME" = "Linux" ]; then
            prompt_and_store_input "LINUX_INPUT" "Do you want to install keploy with Linux or Docker? (linux/docker): "
            if ! sudo mountpoint -q /sys/kernel/debug; then
                sudo mount -t debugfs debugfs /sys/kernel/debug
            fi

            if [ "$LINUX_INPUT" = "linux" ]; then
                if [ "$ARCH" = "x86_64" ]; then
                    install_keploy_amd
                elif [ "$ARCH" = "aarch64" ]; then
                    install_keploy_arm
                else
                    echo "Unsupported architecture: $ARCH"
                    return
                fi
            elif [ "$LINUX_INPUT" = "docker" ]; then
                install_docker
            else
                echo "Please enter a valid command"
                return
            fi
        elif [[ "$OS_NAME" == MINGW32_NT* ]]; then
            echo "\e]8;; https://pureinfotech.com/install-windows-subsystem-linux-2-windows-10\aWindows not supported please run on WSL2\e]8;;\a"
        elif [[ "$OS_NAME" == MINGW64_NT* ]]; then
            echo "\e]8;; https://pureinfotech.com/install-windows-subsystem-linux-2-windows-10\aWindows not supported please run on WSL2\e]8;;\a"
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
            return
        fi
    fi
}

installKeploy

if command -v keploy &>/dev/null; then
    keploy example
    rm keploy.sh
fi