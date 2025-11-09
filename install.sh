#!/bin/bash

installKeploy (){
    version="latest"
    IS_CI=false
    NO_ROOT=false
    PLATFORM="$(basename "$SHELL")"
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
            -noRoot)
                NO_ROOT=true
                shift 1
            ;;
            -platform)
                PLATFORM="$2"
                shift 2
            ;;
            *)
            ;;
        esac
    done
    if [ "$version" != "latest" ]; then
        echo "Installing Keploy version: $version......"
    fi

    move_keploy_binary() {
        # Check if NO_ROOT is set to true
        if [ "$NO_ROOT" = "true" ]; then
            # Move without sudo
            target_dir="$HOME/.keploy/bin"
            source_dir="/tmp/keploy"  # Default source directory

            # Create the target directory in the user's home directory
            mkdir -p "$target_dir"
            if [ $? -ne 0 ]; then
                echo "Error: Failed to create directory $target_dir"
                exit 1
            fi

            # Check if the OS is macOS (Darwin) to set the correct source path
            OS_NAME=$(uname)  # Get the operating system name
            if [ "$OS_NAME" = "Darwin" ]; then
                source_dir="/tmp/keploy/keploy"  # Set source directory to the binary inside the extracted folder
            fi

            # Move the keploy binary to the user's home directory bin
            if [ -f "$source_dir" ]; then
                mv "$source_dir" "$target_dir/keploy"
                if [ $? -ne 0 ]; then
                    echo "Error: Failed to move the keploy binary from $source_dir to $target_dir"
                    exit 1
                fi
            else
                echo "Error: $source_dir does not exist."
                exit 1
            fi

            # Make sure the binary is executable
            chmod +x "$target_dir/keploy"
            if [ $? -ne 0 ]; then
                echo "Error: Failed to make the keploy binary executable"
                exit 1
            fi
        else
            source_dir="/tmp/keploy"
            OS_NAME=$(uname)  # Get the operating system name
            if [ "$OS_NAME" = "Darwin" ]; then
                source_dir="/tmp/keploy/keploy"  # Set source directory to the binary inside the extracted folder
            fi
            sudo mkdir -p /usr/local/bin && sudo mv "$source_dir" /usr/local/bin/keploy
        fi
        set_alias
    }

    install_keploy_darwin_all() {
        if [ "$version" != "latest" ]; then
            download_url="https://github.com/keploy/keploy/releases/download/$version/keploy_darwin_all.tar.gz"
        else
            download_url="https://github.com/keploy/keploy/releases/latest/download/keploy_darwin_all.tar.gz"
        fi
        # macOS tar does not support --overwrite option so we need to remove the directory first
        # to avoid the "File exists" error
        rm -rf /tmp/keploy
        mkdir -p /tmp/keploy
        curl --silent --location "$download_url" | tar xz -C /tmp/keploy/
        move_keploy_binary
        delete_keploy_alias
    }

    install_keploy_arm() {
        if [ "$version" != "latest" ]; then
            download_url="https://github.com/keploy/keploy/releases/download/$version/keploy_linux_arm64.tar.gz"
        else
            download_url="https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz"
        fi
        curl --silent --location "$download_url" | tar xz --overwrite -C /tmp 
        move_keploy_binary
    }


    install_keploy_amd() {        
        if [ "$version" != "latest" ]; then
            download_url="https://github.com/keploy/keploy/releases/download/$version/keploy_linux_amd64.tar.gz"
        else
            download_url="https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz"
        fi
        curl --silent --location "$download_url" | tar xz --overwrite -C /tmp
        move_keploy_binary
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

    update_path() {
        PATH_CMD="export PATH=\"\$HOME/.keploy/bin:\$PATH\""
        rc_file="$1"
        if [ -f "$rc_file" ]; then
            if ! grep -q "$PATH_CMD" "$rc_file"; then
                append_to_rc "$PATH_CMD" "$rc_file"
            fi
        else
            export PATH="$PATH_CMD"
        fi
    }

    # Get the alias to set and set it
    set_alias() {
        current_shell="$PLATFORM"
        if [ "$NO_ROOT" = "true" ]; then
            # Just update the PATH in .zshrc or .bashrc, no alias needed
            if [[ "$current_shell" = "zsh" || "$current_shell" = "-zsh" ]]; then
                update_path "$HOME/.zshrc"
            elif [[ "$current_shell" = "bash" || "$current_shell" = "-bash" ]]; then
                update_path "$HOME/.bashrc"
            else
                update_path "$HOME/.profile"
            fi
        else
            ALIAS_CMD="alias keploy='sudo -E env PATH=\"\$PATH\" keploy'"
            # Handle zsh or bash for non-macOS systems
            if [[ "$current_shell" = "zsh" || "$current_shell" = "-zsh" ]]; then
                if [ -f "$HOME/.zshrc" ]; then
                    if grep -q "alias keploy=" "$HOME/.zshrc"; then
                        sed -i '/alias keploy/d' "$HOME/.zshrc"
                    fi
                    append_to_rc "$ALIAS_CMD" ~/.zshrc
                else
                    alias keploy="$ALIAS_CMD"
                fi
            elif [[ "$current_shell" = "bash" || "$current_shell" = "-bash" ]]; then
                if [ -f "$HOME/.bashrc" ]; then
                    if grep -q "alias keploy=" "$HOME/.bashrc"; then
                        sed -i '/alias keploy/d' "$HOME/.bashrc"
                    fi
                    append_to_rc "$ALIAS_CMD" ~/.bashrc
                else
                    alias keploy="$ALIAS_CMD"
                fi
            else
                if [ -f "$HOME/.profile" ]; then
                    if grep -q "alias keploy=" "$HOME/.profile"; then
                        sed -i '/alias keploy/d' "$HOME/.profile"
                    fi
                    append_to_rc "$ALIAS_CMD" ~/.profile
                else
                    alias keploy="$ALIAS_CMD"
                fi
            fi

        fi
    
    }

    delete_keploy_alias() {
        current_shell="$PLATFORM"
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
                if [ "$NO_ROOT" = "true" ]; then
                    rm -rf "/tmp/$file"
                else
                    sudo rm -rf "/tmp/$file"
                fi
                
            fi
        done
    }

    ARCH=$(uname -m)
    
    OS_NAME="$(uname -s)"
    if [ "$OS_NAME" = "Darwin" ]; then
        NO_ROOT=true
    fi

    if [ "$IS_CI" = false ]; then
        OS_NAME="$(uname -s)"
        if [ "$OS_NAME" = "Darwin" ]; then
            cleanup_tmp
            install_keploy_darwin_all
            return
        elif [ "$OS_NAME" = "Linux" ]; then
             if [ "$NO_ROOT" = false ]; then
                if ! mountpoint -q /sys/kernel/debug; then
                    sudo mount -t debugfs debugfs /sys/kernel/debug
                fi
            fi
            if [ "$ARCH" = "x86_64" ]; then
                cleanup_tmp
                install_keploy_amd
            elif [ "$ARCH" = "aarch64" ]; then
                cleanup_tmp
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
            cleanup_tmp
            install_keploy_amd
        elif [ "$ARCH" = "aarch64" ]; then
            cleanup_tmp
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
    cleanup_tmp
    rm -rf keploy.sh
    rm -rf install.sh
fi