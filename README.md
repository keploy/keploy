[![contributions welcome](https://img.shields.io/badge/contributions-welcome-brightgreen?logo=github)](CODE_OF_CONDUCT.md) 
[![Tests](https://github.com/keploy/keploy/actions/workflows/go.yml/badge.svg)](https://github.com/keploy/keploy/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/keploy/keploy)](https://goreportcard.com/report/github.com/keploy/keploy)
[![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main)](https://coveralls.io/github/keploy/keploy?branch=main)
[![Slack](.github/slack.svg)](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA)
[![License](.github/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

# Keploy
Keploy is a no-code testing platform that generates tests from API calls.

It converts API calls into testcases. Mocks are automatically generated with the actual request/responses. 

<a href="https://www.youtube.com/watch?v=i7OqSVHjY1k"><img alt="link-to-video-demo" src="https://raw.githubusercontent.com/keploy/docs/master/static/img/link-to-demo-video.png" title="Link to Demo Video" width="50%"/></a>


## Features
**Generates test cases** from API calls. Say B-Bye! to writing unit and API test cases.

<img src="https://github.com/keploy/docs/blob/master/static/gif/record-testcase.gif?raw=true" width="100%"  alt="Generate Test Case from API call"/>

**Native interoperability** with popular testing libraries like `go-test`. Code coverage will be reported with existing and Keploy recorded test cases and can also be integrated in CI pipelines/infrastructure.

<img src="https://github.com/keploy/docs/blob/master/static/gif/unit-test.gif?raw=true" width="100%"  alt="Generate Test Case from API call"/>

## Other Features
* **Automatically mock** network/external dependencies with correct responses. No more manually writing mocks for dependencies like DBs, internal services, or third party services like twilio, shopify or stripe.
* **Safely replay writes** or mutations by capturing from local or other environments. Idempotency guarantees are also not required in the application. Multiple Read after write operations can be replicated automatically too.
* **Accurate noise detection** in responses like (timestamps, random values) to ensure high quality tests.
* **Statistical deduplication** ensures that redundant testcases are not generated. We're planning to make this more robust (ref #27).
* **Web Console** to visually understand the results, update behaviour and share findings across your team.
* **Automatic instrumentation** for popular libraries/drivers like sql, http, grpc, etc. 
* **Instrumentation/Integration framework** to easily add the new libraries/drivers with ~100 lines of code.   

## How it works?

![How it works](https://raw.githubusercontent.com/keploy/docs/master/static/img/how-it-works.png)

**Note:** You can generate test cases from **any environment** which has all the infrastructure dependencies setup. Please consider using this to generate tests from low-traffic environments first. The deduplication feature necessary for high-traffic environments is currently experimental.   

## Installation
### Start keploy server
```shell
git clone https://github.com/keploy/keploy.git && cd keploy
docker-compose up
```
The UI can be accessed at http://localhost:8081

### Keeping keploy up-to-date
```shell
docker-compose pull
```

### Integrate the SDK
Install the [Go SDK](https://github.com/keploy/go-sdk) with
```shell
go get -u github.com/keploy/go-sdk
```

## Sample application
```shell
git clone https://github.com/keploy/example-url-shortener && cd example-url-shortener
go mod download
go run handler.go main.go
```

You try generating testcases and testing the sample application:
https://github.com/keploy/example-url-shortener

## Integration with native go test framework
You just need 3 lines of code in your unit test file and that's it!!🔥🔥🔥
```go

import (
	"github.com/keploy/go-sdk/keploy"
	"testing"
)

func TestKeploy(t *testing.T) {
	keploy.SetTestMode()
	go main()
	keploy.AssertTests(t)
}
```
## Language Support
- [x] [Go SDK](https://github.com/keploy/go-sdk)
- [ ] Java SDK - WIP [#51](https://github.com/keploy/keploy/issues/51)
- [ ] Typescript/Javascript SDK - WIP [#61](https://github.com/keploy/keploy/issues/61)
- [ ] Python SDK - WIP [#58](https://github.com/keploy/keploy/issues/58)
- [ ] Need another language support? Please raise an [issue](https://github.com/keploy/keploy/issues/new?assignees=&labels=&template=feature_request.md&title=) or discuss on our [slack channel](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA)

## FAQs
### Is Keploy a unit testing framework? 
No, keploy is designed to reduce time writing tests manually. It integrates with exising unit testing frameworks like (eg: go test, Junit, pytest, etc.) to ensure compatibility with existing tooling like code coverage, IDE support and CI pipeline/infrastructure support.

### Does Keploy replace unit tests entirely?
If all your code paths can be invoked from API calls then yes, else you can still write testcases for some methods, but the idea is to save at least 80% of the effort.  

### What code changes do I need to do?
1. **Web Framework/Router middleware** needs to be added to ensure keploy can intercept incoming request and inject instrumentation data in the request context.
2. **Wrapping External calls** like database queries, http/gRPC calls needs to be done to ensure they are captured and correct mocks are generated for testing those requests.

### How do I run keploy in my CI pipeline? 
No changes necessary. You can reuse the pipeline which runs unit tests. 

### Does Keploy support read after write to DB scenarios?
Yes. Keploy records the write requests and read requests in the correct order. It then expects the application to perform the writes and reads in the same order. It would return the same database responses as captured earlier. 

### How does keploy handle fields like timestamps, random numbers (eg: uuids)? 
A request only becomes a testcase if it passes our deduplication algorithm. If its becoming a testcase, a second request is sent to the same application instance (with the same request params) to check for difference in responses. Fields such as timestamps, uuids would be automatically flagged by comparing the second response with the first response. These fields are then ignored during testing going forward. 

### Can I use keploy to generate tests from production environments automatically? 
Not yet. We are working on making our deduplication algorithm scalable enough to be used safely in production. If you are interested in this use-case, please connect with us on slack. We'd love to work with you to build the deduplication system and load test it with your systems.  

### What if my application behaviour changes? 
If your application behaviour changes, the respective testcases would fail. You can then mark the new behaviour as normal by clicking on the normalise button.   

### Would keploy know if an external service changes? 
Not yet. Unless that application is also using keploy, keploy would only test the functionality of the current application. We are working to detect scanning for API contract violations and adding multiple application to perform comprehensive integration tests. All contributions are welcome.  

## Contributing
There's a separate [docker-compose](docker-compose-dev.yaml) file which helps with exposing the mongo server and also builds the dockerfile from local code.  The `build` flag ensures that the binary is built again to reflect the latest code changes. There's also [docker-compose-debug.yaml](docker-compose-debug.yaml) which can help remote debugging the go server on port 40000.  
```shell
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

