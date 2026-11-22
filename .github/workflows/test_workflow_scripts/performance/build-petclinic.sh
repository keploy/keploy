#!/bin/bash
# build-petclinic.sh: builds the PetClinic jar the performance workflow puts
# under load, in the PetClinic checkout (the working directory), with its own
# Maven wrapper, online, as its README builds it.
#
# The workflow caches the jar under the PetClinic commit, the JDK and this
# file's hash, and runs this only when the cache has no jar for them: a
# change here is a new key, so the next run builds the jar again.
set -euo pipefail
./mvnw clean package -DskipTests -T 1C
