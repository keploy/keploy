#!/bin/bash

set -e

##
## Instructions
##

# 1. Edit the variables below to match your environment. This uses default variables and assumes you're on 5.31.0.
#    If you're wanting to use another version of Postgres/Mattermost , update the variables as desired.

# 2. run 'sudo bash upgrade-from-mattermost-docker.sh' replace upgrade.sh with what you've named the file.
#    This may take some time to complete as it's migrating the database to Postgres 13.6 from 9.4


##
## Environment Variables
##
PATH_TO_MATTERMOST_DOCKER=$PATH_TO_MATTERMOST_DOCKER # i.e. $PWD

# Below are default values in the mattermost-docker container. If you've edited these you will need
# to modify them before running the script or this will generate a new database.
POSTGRES_USER=$POSTGRES_USER # i.e. mmuser
POSTGRES_PASSWORD=$POSTGRES_PASSWORD # i.e. mmuser_password
POSTGRES_DB=$POSTGRES_DB # i.e. mattermost

# You should be on Postgres 9.4. To get your version run
# 'sudo cat volumes/db/var/lib/postgresql/data/PG_VERSION' to confirm this.
POSTGRES_OLD_VERSION=$POSTGRES_OLD_VERSION # i.e. 9.4
POSTGRES_NEW_VERSION=$POSTGRES_NEW_VERSION # i.e. 13

# This tag is found here - https://hub.docker.com/_/postgres'
# This tag needs to be an alpine release to include python3-dev
POSTGRES_DOCKER_TAG=$POSTGRES_DOCKER_TAG # i.e. '13.2-alpine'
POSTGRES_OLD_DOCKERFILE=$POSTGRES_OLD_DOCKERFILE # i.e. `sudo cat $PATH_TO_MATTERMOST_DOCKER/db/Dockerfile | grep 'FROM postgres'`
POSTGRES_NEW_DOCKERFILE=$POSTGRES_NEW_VERSION # i.e. 'FROM postgres:'$POSTGRES_DOCKER_TAG


# This is found here - https://github.com/tianon/docker-postgres-upgrade
# The string here needs to match a folder on that repo. It should read 'old-to-new'.
POSTGRES_UPGRADE_LINE=$POSTGRES_UPGRADE_LINE # i.e. '9.4-to-13'

# Mattermost Versions
MM_OLD_VERSION=$MM_OLD_VERSION # i.e. "5.31.0"
MM_NEW_VERSION=$MM_NEW_VERSION # i.e. "5.32.1"

if [[ $PATH_TO_MATTERMOST_DOCKER == "" ]]; then
  echo "Please set environment variable PATH_TO_MATTERMOST_DOCKER in the script. "
  exit 1
fi
if [[ $POSTGRES_USER == "" ]]; then
  echo "Please set environment variable POSTGRES_USER in the script. "
  exit 1
fi
if [[ $POSTGRES_PASSWORD == "" ]]; then
  echo "Please set environment variable POSTGRES_PASSWORD in the script. "
  exit 1
fi
if [[ $POSTGRES_DB == "" ]]; then
  echo "Please set environment variable POSTGRES_DB in the script. "
  exit 1
fi
if [[ $POSTGRES_OLD_VERSION == "" ]]; then
  echo "Please set environment variable POSTGRES_OLD_VERSION in the script. "
  exit 1
fi
if [[ $POSTGRES_NEW_VERSION == "" ]]; then
  echo "Please set environment variable POSTGRES_NEW_VERSION in the script. "
  exit 1
fi
if [[ $POSTGRES_DOCKER_TAG == "" ]]; then
  echo "Please set environment variable POSTGRES_DOCKER_TAG in the script. "
  exit 1
fi
if [[ $POSTGRES_OLD_DOCKERFILE == "" ]]; then
  echo "Please set environment variable POSTGRES_OLD_DOCKERFILE in the script. "
  exit 1
fi
if [[ $POSTGRES_NEW_DOCKERFILE == "" ]]; then
  echo "Please set environment variable POSTGRES_NEW_DOCKERFILE in the script. "
  exit 1
fi
if [[ $POSTGRES_UPGRADE_LINE == "" ]]; then
  echo "Please set environment variable POSTGRES_UPGRADE_LINE in the script. "
  exit 1
fi
if [[ $MM_OLD_VERSION == "" ]]; then
  echo "Please set environment variable MM_OLD_VERSION in the script. "
  exit 1
fi
if [[ $MM_NEW_VERSION == "" ]]; then
  echo "Please set environment variable MM_NEW_VERSION in the script. "
  exit 1
fi
echo "Path to mattermost-docker: $PATH_TO_MATTERMOST_DOCKER"
echo "Postgres user: $POSTGRES_USER"
echo "Postgres password: $POSTGRES_PASSWORD"
echo "Postgres password: $POSTGRES_DB"
echo "Postgres database name: $POSTGRES_DB"
echo "Postgres old version: $POSTGRES_OLD_VERSION"
echo "Postgres new version: $POSTGRES_NEW_VERSION"
echo "Postgres alpine docker tag including python3-dev: $POSTGRES_DOCKER_TAG"
echo "Postgres old Dockerfile: $POSTGRES_OLD_DOCKERFILE"
echo "Postgres new Dockerfile: $POSTGRES_NEW_DOCKERFILE"
echo "Postgres upgrade-line: $POSTGRES_UPGRADE_LINE"
echo "Mattermost old version: $MM_OLD_VERSION"
echo "Mattermost new version: $MM_NEW_VERSION"
df -h
read -p "Please make sure you have enough disk space left on your devices. Try to backup and upgrade now? (y/n)" choice
if [[ "$choice" != "y" && "$choice" != "Y" && "$choice" && "yes" ]]; then
  exit 0;
fi

##
## Script Start
##
cd $PATH_TO_MATTERMOST_DOCKER
docker-compose stop

# Creating a backup folder and backing up the mattermost / database.
mkdir $PATH_TO_MATTERMOST_DOCKER/backups
cp -ra $PATH_TO_MATTERMOST_DOCKER/volumes/app/mattermost/ $PATH_TO_MATTERMOST_DOCKER/backups/mattermost-backup-$(date +'%F-%H-%M')/
cp -ra $PATH_TO_MATTERMOST_DOCKER/volumes/db/ $PATH_TO_MATTERMOST_DOCKER/backups/database-backup-$(date +'%F-%H-%M')/

mkdir $PATH_TO_MATTERMOST_DOCKER/volumes/db/$POSTGRES_OLD_VERSION
mv $PATH_TO_MATTERMOST_DOCKER/volumes/db/var/lib/postgresql/data/ $PATH_TO_MATTERMOST_DOCKER/volumes/db/$POSTGRES_OLD_VERSION
rm -rf $PATH_TO_MATTERMOST_DOCKER/volumes/db/var
mkdir -p $PATH_TO_MATTERMOST_DOCKER/volumes/db/$POSTGRES_NEW_VERSION/data


sed -i "s/$POSTGRES_OLD_DOCKERFILE/$POSTGRES_NEW_DOCKERFILE/" $PATH_TO_MATTERMOST_DOCKER/db/Dockerfile
sed -i "s/python-dev/python3-dev/" $PATH_TO_MATTERMOST_DOCKER/db/Dockerfile
sed -i "s/$MM_OLD_VERSION/$MM_NEW_VERSION/" $PATH_TO_MATTERMOST_DOCKER/app/Dockerfile


# replacing the old postgres path with a new path
sed -i "s#./volumes/db/var/lib/postgresql/data:/var/lib/postgresql/data#./volumes/db/$POSTGRES_NEW_VERSION/data:/var/lib/postgresql/data#" $PATH_TO_MATTERMOST_DOCKER/docker-compose.yml

# migrate the database to the new postgres version
docker run --rm \
    -e PGUSER="$POSTGRES_USER" \
    -e POSTGRES_INITDB_ARGS=" -U $POSTGRES_USER" \
    -e POSTGRES_PASSWORD="$POSTGRES_PASSWORD" \
    -e POSTGRES_DB="$POSTGRES_DB" \
    -v $PATH_TO_MATTERMOST_DOCKER/volumes/db:/var/lib/postgresql \
    tianon/postgres-upgrade:$POSTGRES_UPGRADE_LINE \
    --link

cp -p $PATH_TO_MATTERMOST_DOCKER/volumes/db/$POSTGRES_OLD_VERSION/data/pg_hba.conf $PATH_TO_MATTERMOST_DOCKER/volumes/db/$POSTGRES_NEW_VERSION/data/

# rebuild the containers
docker-compose build
docker-compose up -d

# reindex the database
echo "REINDEX SCHEMA CONCURRENTLY public;" | docker exec mattermost-docker_db_1 psql -U $POSTGRES_USER $POSTGRES_DB
cd -
