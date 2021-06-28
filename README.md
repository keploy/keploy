# Mattermost Docker setup

This repository is meant to replace the previous *mattermost-docker* repository and tries to stay as close as possible
to the old one. Migrating will include some steps and while trying to keep it simple the structure changed. To keep it
simple all the basic configuration can be done through the *.env* file.

## Prequisites
1. A Docker installation is needed (https://docs.docker.com/engine/install/)
2. Docker-compose needs to be >= 1.28 (https://docs.docker.com/compose/install/)

## Quick setup
These steps are required for new Mattermost setups and don't include everything needed when already using
*mattermost-docker*.

### 1. Cloning the repository (as an alternative please download it as archive)
```
git clone https://github.com/mattermost/docker
cd docker
```

### 2. Create a *.env* file by copying and adjusting the env.example file
Docker will search for an *.env* file when no option specifies another environment file. Afterwards edit it with your preferred text editor.
```
cp env.example .env
```

Within the .env file make sure you edit at a minimum the below values. You can find a list of the Mattermost version tags here: [enterprise-edition](https://hub.docker.com/r/mattermost/mattermost-enterprise-edition/tags?page=1&ordering=last_updated) / [team-edition](https://hub.docker.com/r/mattermost/mattermost-team-edition/tags?page=1&ordering=last_updated). If you want to change the postgres user and pass, you will do so in this `.env` file as well.

```
## This domain must be a live domain that points to the server where Mattermost is installed.
DOMAIN=mm.example.com

## This will be 'mattermost-enterprise-edition' or 'mattermost-team-edition' based on the version of Mattermost you're installing.
MATTERMOST_IMAGE=mattermost-enterprise-edition
MATTERMOST_IMAGE_TAG=5.34
 ```


### 3. Create the needed directores and set permissions (this orientates on the previous *mattermost-docker* structure and the direcories can be changed in the *.env* file)

```
mkdir -p ./volumes/app/mattermost/{config,data,logs,plugins,client-plugins}
sudo chown -R 2000:2000 ./volumes/app/mattermost
```

### 4. Enable Docker
First ensure the docker daemon is enabled and running:
```
sudo systemctl enable --now docker
```

### 5. Placing the certificate and key (if using provided nginx)
Use either 4.1 or 4.2 for setting up SSL. Both methods require you to change the path to the Let's Encrypt config folders inside the *.env*. 


#### 5.1 Pre-existing certificate and key

```
## When using the provided nginx and if a certificate and key already exists make the below directory
mkdir -p ./volumes/web/cert

## Then copy your existing certs into this directory.
cp PATH-TO-CERT.PEM ./volumes/web/cert/cert.pem
cp PATH-TO-KEY.PEM ./volumes/web/cert/key-no-password.pem

```
#### 5.2 Let's Encrypt
**TODO: add link to Let's Encrypt certificate guide**

For using Let's Encrypt you can follow this guide LINK or use the this Bash script scripts/issue-certificate.sh. Make sure to adjust `mm.example.com` to match your domain configured in step 2.
```
bash scripts/issue-certificate.sh -d mm.example.com -o ${PWD}/certs
```

#### 5.3 Adjusting the `.env` file.
Once you've completed 5.1 or 5.2 you'll need to adjust the `.env` file accordingly. With 5.1 verify the first two lines below are uncommented in the `.env` file, with 5.2 comment out the first two lines and uncomment the last two lines.

```
CERT_PATH=./volumes/web/cert/cert.pem
KEY_PATH=./volumes/web/cert/key-no-password.pem
#CERT_PATH=./certs/etc/letsencrypt/live/${DOMAIN}/fullchain.pem
#KEY_PATH=./certs/etc/letsencrypt/live/${DOMAIN}/privkey.pem
```

### 6. Run `docker-compose`
Choose if you're running this docker image with NGINX or using your own proxy.

#### 6.1 Default (with nginx)
```
sudo docker-compose -f docker-compose.yml -f docker-compose.nginx.yml up -d
```

#### 6.2. Without nginx (for use behind an existing reverse proxy)
```
sudo docker-compose -f docker-compose.yml -f docker-compose.without-nginx.yml up -d
```

# Update Mattermost to the latest version

To update Mattermost to the latest version in this repo run the below commands. This will download a new image of the instance and update Mattermost.

```
sudo docker-compose down
git pull

## Based on what you followed in step 6
docker-compose -f docker-compose.yml -f docker-compose.nginx.yml up -d
## OR
docker-compose -f docker-compose.yml -f docker-compose.without-nginx.yml up -d
```

# Installing different versions of Mattermost
If you want to have a different version of Mattermost installed you will need to follow the below steps:

1. Open the `.env` file in your docker folder
2. Edit the line `MATTERMOST_IMAGE_TAG=5.34` to be equal to the version you want. Ex: (`MATTERMOST_IMAGE_TAG=5.35`).
  - You can find a list of the Mattermost version tags here: [enterprise-edition](https://hub.docker.com/r/mattermost/mattermost-enterprise-edition/tags?page=1&ordering=last_updated) / [team-edition](https://hub.docker.com/r/mattermost/mattermost-team-edition/tags?page=1&ordering=last_updated).
3. `sudo docker-compose down`
4. `sudo docker-compose up -d`

# Removing The Docker Containers

Remove the containers

```
docker-compose down
```

Remove the data and settings of your Mattermost instance
```
sudo rm -rf volumes
```
