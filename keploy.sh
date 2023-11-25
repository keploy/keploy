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
        alias keploy='docker run --pull always --name keploy-v2 -p 16789:16789 --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v '"$HOME"'/.keploy-config:/root/.keploy-config -v '"$HOME"'/keploy-config:/root/keploy-config --rm ghcr.io/keploy/keploy'
    }

    install_docker() {
        if ! docker network ls | grep -q 'keploy-network'; then
            docker network create keploy-network
        fi
        alias keploy='sudo docker run --pull always --name keploy-v2 -p 16789:16789 --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v '"$HOME"'/.keploy-config:/root/.keploy-config -v '"$HOME"'/keploy-config:/root/keploy-config --rm ghcr.io/keploy/keploy'
    }

    ARCH=$(uname -m)

    # Renders a text based list of options that can be selected by the
    # user using up, down and enter keys and returns the chosen option.
    #
    #   Arguments   : list of options, maximum of 256
    #                 "opt1" "opt2" ...
    #   Return value: selected index (0 for opt1, 1 for opt2 ...)
    select_option() {
        ESC=$(printf "\033")

        cursor_blink_on() {
            printf "$ESC[?25h"
        }

        cursor_blink_off() {
            printf "$ESC[?25l"
        }

        cursor_to() {
            printf "$ESC[$1;${2:-1}H"
        }

        print_option() {
            printf "   $1 "
        }

        print_selected() {
            printf "  $ESC[7m $1 $ESC[27m"
        }

        get_cursor_row() {
            IFS=';' read -sdR -p $'\E[6n' ROW COL
            echo ${ROW#*[}
        }

        key_input() {
            read -s -n3 key 2>/dev/null >&2
            if [ "$key" = "$ESC[A" ]; then
                echo up
            elif [ "$key" = "$ESC[B" ]; then
                echo down
            elif [ "$key" = "" ]; then
                echo enter
            fi
        }

        options="$1"
        set -- $options  # Set positional parameters
        for opt do
            printf "\n"
        done

        local lastrow
        lastrow=$(get_cursor_row)
        local startrow
        startrow=$((lastrow - $#))

        trap "cursor_blink_on; stty echo; printf '\n'; exit" 2
        cursor_blink_off

        local selected=0
        while true; do
            local idx=0
            for opt; do
                cursor_to $((startrow + idx))
                if [ "$idx" -eq "$selected" ]; then
                    print_selected "$opt"
                else
                    print_option "$opt"
                fi
                idx=$((idx + 1))
            done

            case $(key_input) in
                enter) break;;
                up) selected=$((selected - 1))
                    if [ "$selected" -lt 0 ]; then
                        selected=$(( $# - 1 ))
                    fi;;
                down) selected=$((selected + 1))
                    if [ "$selected" -ge $# ]; then
                        selected=0
                    fi;;
            esac
        done

        cursor_to "$lastrow"
        printf "\n"
        cursor_blink_on

        return "$selected"
    }

    ARCH=$(uname -m)

    if [ "$IS_CI" = false ]; then
        OS_NAME="$(uname -s)"
        if [ "$OS_NAME" = "Darwin" ]; then
        #!/bin/bash
            if ! which docker &> /dev/null; then
                echo -n "Docker not found on device, install docker? (y/n):"
                read user_input
                if [ "$user_input" = "y" ]; then
                    echo "Installing docker via brew"
                    if command -v brew &> /dev/null; then
                       brew install docker
                    else
                        echo "\e]8;;https://brew.sh\abrew is not installed, install brew for easy docker installation\e]8;;\a"
                        return
                    fi
                elif [ "$user_input" != "n" ]; then
                    echo "Please enter a valid command"
                    return
                else
                    echo "Please install docker to install keploy"
                    return
                fi
            fi
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
            echo -n "Do you want to install keploy with Linux or Docker? (select using arrow keys) "
            echo 

            options="linux docker"

            select_option "$options"
            choice="$?" 

            if ! sudo mountpoint -q /sys/kernel/debug; then
                sudo mount -t debugfs debugfs /sys/kernel/debug
            fi
            if [ "$choice" = "0" ]; then
                if [ "$ARCH" = "x86_64" ]; then
                    install_keploy_amd
                elif [ "$ARCH" = "aarch64" ]; then
                    install_keploy_arm
                else
                    echo "Unsupported architecture: $ARCH"
                    return
                fi
            elif [ "$choice" = "1" ]; then
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
    rm keploy.sh
fi