# Keploy

## Quickstart
```shell
docker-compose up
```
The UI can be accessed at http://localhost:8081

## Development
There's a separate [docker-compose](docker-compose-dev.yaml) file which helps which exposes the mongo server and also remote debugging port. The `build` flag ensures that the binary is built again to reflect the latest code changes. 
```shell
git clone https://github.com/keploy/keploy.git && cd keploy
docker-compose -f docker-compose-dev.yaml up --build
```
If you are not using docker, you can build and run the keploy server directly. Ensure to provide the Mongo connection string via the `KEPLOY_MONGO_URI` env variable.  
```shell
export KEPLOY_MONGO_URI="mongodb://mongo:27017"
go run cmd/server/main.go
```
Keploy exposes GraphQL API for the frontend based on [gqlgen](https://github.com/99designs/gqlgen). After changing the [schema](graph/schema.graphqls) you can autogenerate graphQL [handlers](graph/schema.resolvers.go) using
```shell
go generate ./...
```

## Community support
We'd love to collaborate with you to make Keploy great. To get started:
* [Slack](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA) - Discussions with the community and the team.
* [GitHub](https://github.com/keploy/keploy/issues) - For bug reports and feature requests.

