<h1 align="center"> Welcome to Keploy 👋 </h1>

<p style="text-align:center;" align="center">
  <img align="center" src="https://avatars.githubusercontent.com/u/92252339?s=200&v=4" height="20%" width="20%" />
</p>

<p align="center">
  <a href="CODE_OF_CONDUCT.md" alt="Contributions welcome">
    <img src="https://img.shields.io/badge/Contributions-Welcome-brightgreen?logo=github" /></a>
    
  <a href="https://github.com/keploy/keploy/actions" alt="Tests">
    <img src="https://github.com/keploy/keploy/actions/workflows/go.yml/badge.svg" /></a>
    
  <a href="https://goreportcard.com/report/github.com/keploy/keploy" alt="Go Report Card">
    <img src="https://goreportcard.com/badge/github.com/keploy/keploy" /></a>
    
  <a href="https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA" alt="Slack">
    <img src=".github/slack.svg" /></a>
  
  <a href="https://docs.keploy.io" alt="Docs">
    <img src=".github/docs.svg" /></a>
    
  <a href="https://gitpod.io/#https://github.com/keploy/samples-go" alt="Gitpod">
    <img src="https://img.shields.io/badge/Gitpod-ready--to--code-FFB45B?logo=gitpod" /></a>

</p>

# Keploy
Keploy is a functional testing toolkit for developers. It **generates E2E tests for APIs (KTests)** along with **mocks or stubs(KMocks)** by recording real API calls.
KTests can be imported as mocks for consumers and vice-versa.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/how-keploy-works.gif" width="100%"  alt="Generate Test Case from API call"/>

Merge KTests with unit testing libraries(like Go-Test, JUnit..) to track combined test-coverage.

KMocks can also be referenced in existing tests or use anywhere (including any testing framework). KMocks can also be used as tests for the server.   

> Keploy is testing itself with &nbsp;  [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;  without writing many test-cases or data-mocks. 😎

[//]: # (<a href="https://www.youtube.com/watch?v=i7OqSVHjY1k"><img alt="link-to-video-demo" src="https://raw.githubusercontent.com/keploy/docs/main/static/img/link-to-demo-video.png" title="Link to Demo Video" width="50%" heigth="50%"/></a>)

## Language Support
- [x] [![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)](https://github.com/keploy/go-sdk)
- [x] [![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)](https://github.com/keploy/java-sdk)
- [x] [![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)](https://github.com/keploy/typescript-sdk)
- [ ] ![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54) : WIP [#58](https://github.com/keploy/keploy/issues/58)


## How it works?
#### Safely replays all CRUD operations (including non-idempotent APIs)

Keploy is added as a middleware to your application that captures and replays all network interaction served to application from any source.

Visit [https://docs.keploy.io](https://docs.keploy.io/docs/keploy-explained/how-keploy-works) to read more in detail..


<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="80%"  alt="Generate Test Case from API call"/>

## Documentation

#### Here you can find the complete [Documentation](https://docs.keploy.io/) which you can reffer 

## Contributing
Whether you are a community member or not, we would love your point of view! Feel free to first check out our
- [contribution guidelines](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md) 
- The guide outlines the process for **creating an issue** and **submitting a pull request.**
- [code of conduct](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)
- By following the guide we've set, your contribution will more likely be accepted if it enhances the project.

## Features

### 1. Export tests and mocks and maintain alongside existing tests

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="80%"  alt="Generate Test Case from API call"/>

### 2.  Integrates with `go-test`, `junit`
Keploy has native interoperability as it integrates with popular testing libraries like `go-test`, `junit`. 
Code coverage will be reported with existing plus KTests. It'll also be **integrated in CI pipelines/infrastructure automatically** if you already have `go-test`, `junit` integrated.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/replay-tc.gif" width="80%"  alt="Generate Test Case from API call"/>

### 3. Accurate Noise Detection
Filters noisy fields in API responses like (timestamps, random values) to ensure high quality tests.

**WIP** - **Statistical deduplication** ensures that redundant testcases are not generated. WIP (ref [#27](https://github.com/keploy/keploy/issues/27)).

## Quick Installation

### MacOS 

```shell
curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_darwin_all.tar.gz" | tar xz -C /tmp

sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin && keploy
```

### Linux

<details>
<summary>Linux</summary>

```shell
curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz" | tar xz -C /tmp

sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin && keploy
```
</details>

<details>
<summary>Linux ARM</summary>

```shell
curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz" | tar xz -C /tmp

sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin && keploy
```
</details>

### Windows

<details>
<summary>Windows</summary>


- Download the [Keploy Windows AMD64](https://github.com/keploy/keploy/releases/latest/download/keploy_windows_amd64.tar.gz), and extract the files from the zip folder.

- Run the `keploy.exe` file.

</details>

<details>
<summary>Windows ARM</summary>

- Download the [Keploy Windows ARM64](https://github.com/keploy/keploy/releases/latest/download/keploy_windows_arm64.tar.gz), and extract the files from the zip folder.

- Run the `keploy.exe` file.

</details>

## SDK Integration
After running Keploy Server, **let's integrate the SDK** into the application. 
If you're integrating in custom project please choose installation [documentation according to the language](https://docs.keploy.io/application-development/) you're using.


[![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)](https://docs.keploy.io/docs/go/installation)
[![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)](https://docs.keploy.io/docs/java/installation)
[![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)](https://docs.keploy.io/docs/typescript/installation)


## Try Sample application

Demos using *Echo/PostgreSQL* and *Gin/MongoDB* are available [here](https://github.com/keploy/samples-go). For this example, we will use the **Echo/PostgreSQL** sample.

```bash
git clone https://github.com/keploy/samples-go && cd samples-go/echo-sql
go mod download
```

#### Start PostgreSQL instance
```bash
docker-compose up -d
```

#### Run the application
```shell
export KEPLOY_MODE=record && go run handler.go main.go
```

### Generate testcases
To genereate testcases we just need to make some API calls. You can use [Postman](https://www.postman.com/), [Hoppscotch](https://hoppscotch.io/), or simply `curl`

> Note : KTests are exported as files in the current directory(.) by default

#### 1. Generate shortened url
```shell
curl --request POST \
  --url http://localhost:8082/url \
  --header 'content-type: application/json' \
  --data '{
  "url": "https://github.com"
}'
```
this will return the shortened url. The ts would automatically be ignored during testing because it'll always be different.
```json
{
	"ts": 1647802058801841100,
	"url": "http://localhost:8082/GuwHCgoQ"
}
```
#### 2. Redirect to original url from shortened url
```bash
curl --request GET \
  --url http://localhost:8082/GuwHCgoQ
```

### Integration with native Go test framework
You just need 3 lines of code in your unit test file and that's it!!🔥🔥🔥

For an example, for a file named `main.go` create a unit test file as `main_test.go` in the **same folder** as `main.go`.

Contents of `main_test.go`:
```go
package main

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

### Run the testcases
**Note: Before running tests stop the sample application**
```shell
go test -coverpkg=./... -covermode=atomic  ./...
```
this should show you have 74.4% coverage without writing any code!
```shell
ok      echo-psql-url-shortener 5.820s  coverage: 74.4% of statements in ./...
```

The Test Run can be visualised in the terminal where Keploy server is running. You can also checkout the details of the 
Test Run Report as a report file generated locally in the Keploy Server directory.  

## Keploy SDK Modes
### SDK Modes
The Keploy SDKs modes can operated by setting `KEPLOY_MODE` environment variable

> *Note: KEPLOY_MODE value is case sensitive*

There are 3 Keploy SDK modes:

1. **Off** : In the off mode the Keploy SDK will turn off all the functionality provided by the Keploy platform.

```
export KEPLOY_MODE="off"
```
2. **Record mode** :
	* Record requests, response and all external calls and sends to Keploy server.
	* After keploy server removes duplicates, it then runs the request on the API again to identify noisy fields.
	* Sends the noisy fields to the keploy server to be saved along with the testcase.

```
export KEPLOY_MODE="record"
```
3. **Test mode** :
	* Fetches testcases for the app from keploy server.
	* Calls the API with same request payload in testcase.
	* Mocks external calls based on data stored in the testcase.
	* Validates the responses and uploads results to the keploy server
```
export KEPLOY_MODE="test"
```

Need another language support? Please raise an [issue](https://github.com/keploy/keploy/issues/new?assignees=&labels=&template=feature_request.md&title=) or discuss on our [slack channel](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA)

## Quickstart on GitPod
The fastest way to start with Keploy is the Gitpod-hosted version. When you're ready, you can install locally or host yourself.

One-click deploy sample URL Shortener application sample with Keploy using Gitpod

[![Open in Gitpod](https://gitpod.io/button/open-in-gitpod.svg)](https://gitpod.io/#https://github.com/keploy/samples-go)

##  Current Limitations
* **Unit Testing**: While Keploy is designed to run alongside unit testing frameworks (Go test, JUnit..) and can add to the overall code coverage, it still generates E2E tests. So it might be easier to write unit tests for some methods instead of E2E tests. 
* **Production usage** Keploy is currently focused on generating tests for developers. These tests can be captured from any environment, but we have not tested it on high volume production environments. This would need robust deduplication to avoid too many redundant tests being captured. We do have ideas on building a robust deduplication system [#27](https://github.com/keploy/keploy/issues/27) 
* **De-noise requires mocking** Keploy issues a duplicate request and compares the responses with the previous responses to find "noisy" or non-deterministic fields. We have to ensure all non-idempotent dependencies are mocked/wrapped by Keploy to avoid unnecessary side effects in downstream services.  

## Resources
🤔 [FAQs](https://docs.keploy.io/docs/keploy-explained/faq)

🕵️‍️ [Why Keploy](https://docs.keploy.io/docs/keploy-explained/why-keploy)

⚙️ [Installation Guide](https://docs.keploy.io/docs/server/server-installation)

📖 [Contribution Guide](https://docs.keploy.io/docs/devtools/server-contrib-guide/)


## Community Support  ❤️

We'd love to collaborate with you to make Keploy great. To get started:
* [Slack](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA) - Discussions with the community and the team.
* [GitHub](https://github.com/keploy/keploy/issues) - For bug reports and feature requests.

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![Twitter](https://img.shields.io/badge/Twitter-%231DA1F2.svg?style=for-the-badge&logo=Twitter&logoColor=white)](https://twitter.com/Keployio)

<!-- markdownlint-restore -->
<!-- prettier-ignore-end -->
<!-- ALL-CONTRIBUTORS-LIST:END -->
