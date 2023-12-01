#! /bin/bash

# Update the java version
sudo apt update
sudo apt install openjdk-21-jre -y
export JAVA_HOME=/usr/lib/jvm/java-21-openjdk-amd64