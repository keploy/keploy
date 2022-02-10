# Keploy


# Introduction to Keploy

**[Keploy](https://keploy.io)** is an **[open-source](https://github.com/keploy/keploy)** API testing platform that eliminates the need for writing unit cases by
**automatically generating unit test-cases from API calls and mocking external dependencies/libraries**.

## Why Keploy?

![Difference](https://raw.githubusercontent.com/keploy/docs/master/static/img/difference.png)


* Generates test-case automatically when an API call happens. Say, B-Bye! to writing unit test cases.

* Mocks all your external libraries/dependencies while testing. Any dependency like DBs, other internal services, third party libraries like twilio, shopify, stripe behaviour/results are recorded automatically.


* **Supports non-Idempotency** : All WRITE, DELETE, UPDATE operations are mocked and can replayed automatically.


* Test Coverage can be increased to more than 90% from production traffic or lower environments.

* Maintenance effort of unit-testing is reduced and anyone can maintain the test suite since test-cases can be updated from the console(no-code required).


## How it works?

![How it works](https://raw.githubusercontent.com/keploy/docs/master/static/img/how-it-works.png)

### Step 1 : Record network interactions with Keploy SDK

Integrate Keploy SDK with the application. Start the SDK in record mode to capture API calls as test cases.

```bash
export KEPLOY_SDK_MODE="record"
```

Now, when the application serves an API, all of the unique network interactions are stored within Keploy as a test-case.

### Step 2 :  Replay Test-suite locally

Let's say you developed new application version v2. To test locally, start the SDK in test mode to replay all recorded API calls/test cases.

```bash
export KEPLOY_SDK_MODE="test"
```

Now, when the application starts, keploy will download all the previously recorded test-cases/API calls with dependency responses and a report will be generated on the Keploy console.

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

