#!/bin/bash

installKeploy (){
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

        sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin/keploybin
        
        set_alias
    }

    install_keploy_amd() {
        curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz" | tar xz -C /tmp

        sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin/keploybin
        
        set_alias
    }

    set_alias() {
        ALIAS_CMD="alias keploy='sudo -E env PATH=\$PATH keploybin'"

        if [ -f ~/.zshrc ]; then
            if grep -q "alias keploy=" ~/.zshrc; then
                sed -i '/keploy/d' ~/.zshrc
                echo "$ALIAS_CMD" >> ~/.zshrc
            fi
            source ~/.zshrc
        elif [ -f ~/.bashrc ]; then
            if grep -q "alias keploy=" ~/.bashrc; then
               sed -i '/keploy/d' ~/.bashrc
                echo "$ALIAS_CMD" >> ~/.bashrc
            fi
            source ~/.bashrc
        else
            alias keploy='sudo -E env PATH=$PATH keploybin'
        fi
    }


    install_colima_docker() {
        if ! docker network ls | grep -q 'keploy-network'; then
            docker network create keploy-network
        fi
        alias keploy='docker run --pull always --name keploy-v2 -p 16789:16789 --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v '"$HOME"'/.keploy-config:/root/.keploy-config -v '"$HOME"'/keploy-config:/root/keploy-config --rm ghcr.io/keploy/keploy'
    }

    install_docker() {
        if ! docker network ls | grep -q 'keploy-network'; then
            docker network create keploy-network
        fi
        alias keploy='sudo docker run --pull always --name keploy-v2 -p 16789:16789 --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v '"$HOME"'/.keploy-config:/root/.keploy-config -v '"$HOME"'/keploy-config:/root/keploy-config --rm ghcr.io/keploy/keploy'
    }

    ARCH=$(uname -m)

    if [ "$IS_CI" = false ]; then
        OS_NAME="$(uname -s)"
        if [ "$OS_NAME" = "Darwin" ]; then
        #!/bin/bash
            echo -e "Keploy isn't supported on Docker Desktop, \e]8;;https://github.com/docker/for-mac/issues/6800\aknow why?\e]8;;\a"
            if ! which colima &> /dev/null; then
                echo
                echo -e "\e]8;;https://kumojin.com/en/colima-alternative-docker-desktop\aAlternate is to use colima(lightweight and performant alternative to Docker Desktop)\e]8;;\a"
                echo -n "Install colima (y/n):"
                read user_input
                if [ "$user_input" = "y" ]; then 
                    echo "Installing colima via brew"
                    if command -v brew &> /dev/null; then
                        brew install colima
                    else
                        echo "\e]8;;https://brew.sh\abrew is not installed, install brew for easy colima installation\e]8;;\a"
                        return
                    fi
                elif [ "$user_input" = "n" ]; then 
                    echo "Please install Colima to install Keploy."
                    return
                else
                    echo "Please enter a valid command"
                    return
                fi
            else
                echo -n "colima found on your system, would you like to proceed with it? (y/n):"
                read user_input
                if [ "$user_input" = "n" ]; then 
                    echo "Please allow Colima to run Keploy."
                    return
                elif [ "$user_input" != "y" ]; then 
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
            return
        elif [ "$OS_NAME" = "Linux" ]; then
            echo -n "Do you want to install keploy with Linux or Docker? (linux/docker): "
            read user_input
            if ! mountpoint -q /sys/kernel/debug; then
                sudo mount -t debugfs debugfs /sys/kernel/debug
            fi
            if [ "$user_input" = "linux" ]; then
                if [ "$ARCH" = "x86_64" ]; then
                    install_keploy_amd
                elif [ "$ARCH" = "aarch64" ]; then
                    install_keploy_arm
                else
                    echo "Unsupported architecture: $ARCH"
                    return
                fi
            elif [ "$user_input" = "docker" ]; then 
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

if command -v keploy &> /dev/null; then
    keploy example
fi