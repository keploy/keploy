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
$ git clone https://github.com/mattermost/docker
$ cd docker
```

### 2. Create a *.env* file by copying and adjusting the env.example file
Docker will search for an *.env* file when no option specifies another environment file. Afterwards edit it with your preferred text editor.
```
$ cp env.example .env
```

### 3. Create the needed directores and set permissions (this orientates on the previous *mattermost-docker* structure and the direcories can be changed in the *.env* file)

```
$ mkdir -p ./volumes/app/mattermost/{config,data,logs,plugins,client-plugins}
$ sudo chown -R 2000:2000 ./volumes/app/mattermost

## (optinally) when using the provided nginx and if a certificate and key already exists
$ mkdir -p ./volumes/web/cert
```

### 4. Placing the certificate and key (if using provided nginx)
#### 4.1 Pre-existing certificate and key
```
$ cp PATH-TO-CERT.PEM ./volumes/web/cert/cert.pem
$ cp PATH-TO-KEY.PEM ./volumes/web/cert/key-no-password.pem
```
#### 4.2 Let's Encrypt
**TODO: add link to Let's Encrypt certificate guide**

For using Let's Encrypt you can follow this guide LINK or use the this Bash script scripts/issue-certificate.sh. Both
methods requires you to change the path to the Let's Encrypt config folders inside the *.env*.
```
$ sudo docker volume create shared-webroot
$ bash scripts/issue-certificate.sh -d mm.example.com -o ./certs
```

### 5. Run `docker-compose`
First ensure the docker daemon is enabled and running:
```
$ sudo systemctl enable --now docker
```

#### 5.1 Default (with nginx)
```
$ sudo /usr/local/bin/docker-compose -f docker-compose.yml -f docker-compose.nginx.yml up -d
```

#### 5.2. Without nginx (for use behind an existing reverse proxy)
```
$ sudo /usr/local/bin/docker-compose -f docker-compose.yml -f docker-compose.without-nginx.yml up -d
```
