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

    install_keploy() {
        os_name="$1"
        arch="$2"

        case $os_name in
            "Darwin")
                download_url="https://github.com/keploy/keploy/releases/$( [ "$version" != "latest" ] && echo "download/$version" || echo "latest/download" )/keploy_darwin_all.tar.gz"
                ;;
            "Linux")
                if [ "$arch" = "x86_64" ]; then
                    download_url="https://github.com/keploy/keploy/releases/$( [ "$version" != "latest" ] && echo "download/$version" || echo "latest/download" )/keploy_linux_amd64.tar.gz"
                elif [ "$arch" = "aarch64" ]; then
                    download_url="https://github.com/keploy/keploy/releases/$( [ "$version" != "latest" ] && echo "download/$version" || echo "latest/download" )/keploy_linux_arm64.tar.gz"
                else
                    echo "Unsupported architecture: $arch"
                    return 1
                fi
                ;;
            *)
                echo "Unsupported OS: $os_name"
                return 1
                ;;
        esac

        rm -rf /tmp/keploy
        mkdir -p /tmp/keploy
        curl --silent --location "$download_url" | tar xz -C /tmp/keploy/
        sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy/keploy /usr/local/bin/keploy
        delete_keploy_alias
    }

    append_to_rc() {
        last_byte=$(tail -c 1 "$2")
        if [[ "$last_byte" != "" ]]; then
            echo -e "\n$1" >> "$2"
        else
            echo "$1" >> "$2"
        fi
        source "$2"
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

    cleanup_tmp() {
        # Remove extracted files /tmp directory
        tmp_files=("LICENSE" "README.md" "READMEes-Es.md" "README-UnitGen.md")
        for file in "${tmp_files[@]}"; do
            if [ -f "/tmp/$file" ]; then
                sudo rm -rf "/tmp/$file"
            fi
        done
    }

    ARCH=$(uname -m)
    OS_NAME="$(uname -s)"

    cleanup_tmp
    install_keploy "$OS_NAME" "$ARCH"
}

installKeploy "$@"

if command -v keploy &> /dev/null; then
    keploy example
    cleanup_tmp
    rm -rf keploy.sh
    rm -rf install.sh
fi
