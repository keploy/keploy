#! /bin/bash

java_major_version() {
  "$1" -version 2>&1 | awk -F '[\".]' '/version/ {print ($2 == "1" ? $3 : $2); exit}'
}

has_java_17_jdk() {
  [[ -x "$1/bin/java" && -x "$1/bin/javac" ]] && [[ "$(java_major_version "$1/bin/java")" == "17" ]]
}

java_setup_done=false

if [[ -n "${JAVA_HOME:-}" ]]; then
  if has_java_17_jdk "$JAVA_HOME"; then
    export PATH="${JAVA_HOME}/bin:${PATH}"
    java_setup_done=true
  fi
fi

if [[ "$java_setup_done" != "true" ]] && command -v java >/dev/null 2>&1; then
  java_bin="$(command -v java)"
  resolved_java_bin="$(readlink -f "$java_bin" 2>/dev/null || true)"
  if [[ -n "$resolved_java_bin" ]]; then
    java_bin="$resolved_java_bin"
  fi
  java_home="$(dirname "$(dirname "$java_bin")")"

  if has_java_17_jdk "$java_home"; then
    export JAVA_HOME="$java_home"
    export PATH="${JAVA_HOME}/bin:${PATH}"
    java_setup_done=true
  fi
fi

if [[ "$java_setup_done" != "true" ]]; then
  sudo apt-get update
  sudo apt-get install openjdk-17-jdk-headless -y
  export JAVA_HOME=/usr/lib/jvm/java-17-openjdk-amd64
  export PATH="${JAVA_HOME}/bin:${PATH}"
fi
