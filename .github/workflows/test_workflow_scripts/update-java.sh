#! /bin/bash
echo "System Kernel Version: $(uname -r)"

# Update the java version
sudo apt update
sudo apt install openjdk-17-jre -y
export JAVA_HOME=/usr/lib/jvm/java-17-openjdk-amd64
export PATH=$JAVA_HOME/bin:$PATH
source ~/.bashrc