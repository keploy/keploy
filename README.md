[![contributions welcome](https://img.shields.io/badge/contributions-welcome-brightgreen?logo=github)](CODE_OF_CONDUCT.md) 
[![Tests](https://github.com/keploy/keploy/actions/workflows/go.yml/badge.svg)](https://github.com/keploy/keploy/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/keploy/keploy)](https://goreportcard.com/report/github.com/keploy/keploy)
[![codecov](https://codecov.io/gh/keploy/keploy/branch/main/graph/badge.svg?token=ILUM3CXWG5)](https://codecov.io/gh/keploy/keploy)
[![Slack](.github/slack.svg)](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA)
[![License](.github/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

# Keploy
Keploy is a no-code testing platform that generates tests from API calls.

It converts API calls into testcases. Mocks are automatically generated with the actual request/responses. 

## Features
* **Generates test cases** from API calls. Say B-Bye! to writing unit and API test cases.
* **Automatically mock** network/external dependencies with correct responses. No more manually writing mocks for dependencies like DBs, internal services, or third party services like twilio, shopify or stripe.
* **Safely replay writes** or mutations by capturing from local or other environments. Idempotency guarantees are also not required in the application. Multiple Read after write operations can be replicated automatically too.
* **Statistical deduplication** ensures that redundant testcases are not generated. We're planning to make this more robust (ref #27).
* **Web Console** to visually understand the results, update behaviour and share findings across your team.
* **Native interoperability** with popular testing libraries like go-test to enable compatibility with existing test cases and CI pipelines/infrastructure.
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

### Integrate the SDK
Install the [Go SDK](https://github.com/keploy/go-sdk) with
```shell
go get -u github.com/keploy/go-sdk
```

## Example
### Sample application
You can try out the sample application with the go SDK integrated [here](https://github.com/keploy/example-url-shortener) 

#### Routers
Example of integrating the [gin router](https://github.com/gin-gonic/gin). Other [routers](https://github.com/keploy/go-sdk#supported-routers) like echo, chi, etc are support too.
```go
import (
        "github.com/gin-gonic/gin"
        "github.com/keploy/go-sdk/integrations/kgin/v1"
        "github.com/keploy/go-sdk/keploy"
        )

r := gin.New()
port := "6060"

k := keploy.New(keploy.Config{
    App: keploy.AppConfig{
        // your application
        Name: "my-app",
        Port: port,
    },
    Server: keploy.ServerConfig{
        URL: "http://localhost:8081/api",
    },
})
//Call kgin.GinV1 before routes handling
kgin.GinV1(k, r)

r.Run(":" + port)
```

#### Datastore
Example of integrating the official [mongo driver](https://github.com/mongodb/mongo-go-driver). Other [datastore/database](https://github.com/keploy/go-sdk#supported-databases) libraries like go's sql   
```go
import (
        "go.mongodb.org/mongo-driver/mongo"
        "github.com/keploy/go-sdk/integrations/kmongo"
        )

db  := client.Database("MyDB")

// wrap collection with keploy for automatic instrumentation and mocking
col := kmongo.NewCollection(db.Collection("MyCollection"))

```

Thats it! All the requests after integration will be automatically captured and available it at http://localhost:8081/testlist

#### Integration with native go test framework
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

[//]: # (- [ ] Java SDK &#40;coming soon&#41;)

[//]: # (- [ ] Javascript &#40;coming soon&#41;)
- [ ] Need another language support? Please raise an [issue](https://github.com/keploy/keploy/issues/new?assignees=&labels=&template=feature_request.md&title=) or discuss on our [slack channel](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA)
## Development
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

