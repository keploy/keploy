#!/bin/bash

# Path to the CA certificate
caCertPath="./ca.crt"

# Paths to check for CA store
caStorePaths=(
  "/usr/local/share/ca-certificates/"
  "/etc/pki/ca-trust/source/anchors/"
  "/etc/ca-certificates/trust-source/anchors/"
  "/etc/pki/trust/anchors/"
  "/usr/local/share/certs/"
  "/etc/ssl/certs/"
)
# Commands to update the CA store
caStoreUpdateCmds=(
  "update-ca-certificates"
  "update-ca-trust"
  "trust extract-compat"
  "update-ca-trust extract"
  "certctl rehash"
)

# Java related variables
storePass="changeit"
alias="keployCA"

# Check if directory exists
directory_exists() {
  [[ -d $1 ]]
}

# Check if command exists
command_exists() {
  command -v $1 &> /dev/null
}

# Check if Java is installed
is_java_installed() {
  command_exists "java"
}

# Update the CA store
update_ca_store() {
  for cmd in "${caStoreUpdateCmds[@]}"; do
    if command_exists "$cmd"; then
      $cmd
    fi
  done
}

# Install Java CA
install_java_ca() {
  caPath=$1
  
  if is_java_installed; then
    javaHome=$(java -XshowSettings:properties -version 2>&1 > /dev/null | grep 'java.home' | awk -F'=' '{print $2}' | xargs)
    cacertsPath="${javaHome}/lib/security/cacerts"
    
    keytool -list -keystore $cacertsPath -storepass $storePass -alias $alias &> /dev/null
    if [ $? -eq 0 ]; then
      echo "Java detected and CA already exists, cacertsPath:$cacertsPath"
      return
    fi
    
    keytool -import -trustcacerts -keystore $cacertsPath -storepass $storePass -noprompt -alias $alias -file $caPath
    if [ $? -eq 0 ]; then
      echo "Java detected and successfully imported CA, path:$cacertsPath"
    else
      echo "Java detected but failed to import CA"
    fi
  else
    echo "Java is not installed on the system"
  fi
}

# Handle TLS setup
handle_tls_setup() {
  for path in "${caStorePaths[@]}"; do
    if directory_exists "$path"; then
      caPath="${path}ca.crt"
      cp $caCertPath $caPath
      install_java_ca $caPath
    fi
  done
  update_ca_store
  
# Set the NODE_EXTRA_CA_CERTS environment variable
  export NODE_EXTRA_CA_CERTS="/tmp/ca.crt"
  cat $caCertPath > $NODE_EXTRA_CA_CERTS

# Change the file permissions to allow read and write access for all users
  chmod 0666 $NODE_EXTRA_CA_CERTS

# Log the NODE_EXTRA_CA_CERTS environment variable
  echo "NODE_EXTRA_CA_CERTS is set to: $NODE_EXTRA_CA_CERTS"

  # Set the REQUESTS_CA_BUNDLE to the same value as NODE_EXTRA_CA_CERTS for python
  export REQUESTS_CA_BUNDLE=$NODE_EXTRA_CA_CERTS

  # Log the REQUESTS_CA_BUNDLE environment variable
  echo "REQUESTS_CA_BUNDLE is set to: $REQUESTS_CA_BUNDLE"

  echo "Setup successful"
}

# Execute the main function
handle_tls_setup