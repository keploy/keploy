#!/bin/bash

installKeploy (){
    version="latest"
    IS_CI=false
    for arg in "$@"
    do
        case $arg in
            -isCI)
                IS_CI=true
                shift
            ;;
            -v)
                if [[ "$2" =~ ^v[0-9]+.* ]]; then
                    version="$2"
                    shift 2 
                else
                    echo "Invalid version format. Please use '-v v<semver>'."
                    return 1 
                fi
            ;;
            *)
            ;;
        esac
    done

    if [ "$version" != "latest" ]; then
        echo "Installing Keploy version: $version......"
    fi

    install_keploy_darwin_all() {
        if [ "$version" != "latest" ]; then
            download_url="https://github.com/keploy/keploy/releases/download/$version/keploy_darwin_all.tar.gz"
        else
            download_url="https://github.com/keploy/keploy/releases/latest/download/keploy_darwin_all.tar.gz"
        fi

        curl --silent --location "$download_url" | tar xz -C /tmp
        sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin/keploy
        delete_keploy_alias
    }

    install_keploy_arm() {
        if [ "$version" != "latest" ]; then
            download_url="https://github.com/keploy/keploy/releases/download/$version/keploy_linux_arm64.tar.gz"
        else
            download_url="https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz"
        fi
        curl --silent --location "$download_url" | tar xz -C /tmp
        sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin/keploy
        set_alias 'sudo -E env PATH="$PATH" keploy'
    }


    install_keploy_amd() {        
        if [ "$version" != "latest" ]; then
            download_url="https://github.com/keploy/keploy/releases/download/$version/keploy_linux_amd64.tar.gz"
        else
            download_url="https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz"
        fi
        curl --silent --location "$download_url" | tar xz -C /tmp
        sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin/keploybin
        set_alias 'sudo -E env PATH="$PATH" keploy'
    }

    append_to_rc() {
        last_byte=$(tail -c 1 $2)
        if [[ "$last_byte" != "" ]]; then
            echo -e "\n$1" >> $2
        else
            echo "$1" >> $2
        fi
        source $2
    }

    # Get the alias to set and set it
    set_alias() {
        ALIAS_CMD="alias keploy='$1'"
        current_shell="$(basename "$SHELL")"
        if [[ "$current_shell" = "zsh" || "$current_shell" = "-zsh" ]]; then
            if [ -f ~/.zshrc ]; then
                if grep -q "alias keploy=" ~/.zshrc; then
                    if [ "$OS_NAME" = "Darwin" ]; then
                        sed -i '' '/alias keploy/d' ~/.zshrc
                    else
                        sed -i '/alias keploy/d' ~/.zshrc
                    fi
                fi
                append_to_rc "$ALIAS_CMD" ~/.zshrc
            else
                alias keploy="$1"
            fi
        elif [[ "$current_shell" = "bash" || "$current_shell" = "-bash" ]]; then
            if [ -f ~/.bashrc ]; then
                if grep -q "alias keploy=" ~/.bashrc; then
                    if [ "$OS_NAME" = "Darwin" ]; then
                        sed -i '' '/alias keploy/d' ~/.bashrc
                    else
                        sed -i '/alias keploy/d' ~/.bashrc
                    fi
                fi
                append_to_rc "$ALIAS_CMD" ~/.bashrc
            else
                alias keploy="$1"
            fi
        else
            alias keploy="$1"
        fi
    }

    delete_keploy_alias() {
        current_shell="$(basename "$SHELL")"
        shell_rc_file=""
        # Determine the shell configuration file based on the current shell
        if [[ "$current_shell" = "zsh" || "$current_shell" = "-zsh" ]]; then
            shell_rc_file="$HOME/.zshrc"
        elif [[ "$current_shell" = "bash" || "$current_shell" = "-bash" ]]; then
            shell_rc_file="$HOME/.bashrc"
        else
            echo "Unsupported shell: $current_shell"
            return
        fi
        # Delete alias from the shell configuration file if it exists
        if [ -f "$shell_rc_file" ]; then
            if grep -q "alias keploy=" "$shell_rc_file"; then
                if [[ "$(uname)" = "Darwin" ]]; then
                    sed -i '' '/alias keploy/d' "$shell_rc_file"
                else
                    sed -i '/alias keploy/d' "$shell_rc_file"
                fi
            fi
        fi
        # Unset the alias in the current shell session if it exists
        if alias keploy &>/dev/null; then
            unalias keploy
        fi
    }

    ARCH=$(uname -m)

    if [ "$IS_CI" = false ]; then
        OS_NAME="$(uname -s)"
        if [ "$OS_NAME" = "Darwin" ]; then
            install_keploy_darwin_all
            return
        elif [ "$OS_NAME" = "Linux" ]; then
            if ! sudo mountpoint -q /sys/kernel/debug; then
                sudo mount -t debugfs debugfs /sys/kernel/debug
            fi
            if [ "$ARCH" = "x86_64" ]; then
                install_keploy_amd
            elif [ "$ARCH" = "aarch64" ]; then
                install_keploy_arm
            else
                echo "Unsupported architecture: $ARCH"
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

installKeploy "$@"

if command -v keploy &> /dev/null; then
    keploy example
    rm -rf keploy.sh
    rm -rf install.sh
fi
