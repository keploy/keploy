# Welcome to Keploy 👋

<p style="text-align:center;" align="center">
  <img align="center" src="https://avatars.githubusercontent.com/u/92252339?s=200&v=4" height="20%" width="20%" />
</p>

[![contributions welcome](https://img.shields.io/badge/contributions-welcome-brightgreen?logo=github)](CODE_OF_CONDUCT.md) 
[![Tests](https://github.com/keploy/keploy/actions/workflows/go.yml/badge.svg)](https://github.com/keploy/keploy/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/keploy/keploy)](https://goreportcard.com/report/github.com/keploy/keploy)
[![Slack](.github/slack.svg)](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA)
[![Docs](.github/docs.svg)](https://docs.keploy.io)


# Keploy
Keploy is a no-code API testing platform that generates tests-cases and data-mocks from API calls.

Dependency-mocks are automatically generated with the recorded request/responses. 

> Keploy is testing itself with &nbsp;  [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main)](https://coveralls.io/github/keploy/keploy?branch=main) &nbsp;  without writing any test-cases and data-mocks. 😎
<a href="https://www.youtube.com/watch?v=i7OqSVHjY1k"><img alt="link-to-video-demo" src="https://raw.githubusercontent.com/keploy/docs/main/static/img/link-to-demo-video.png" title="Link to Demo Video" width="50%"/></a>


## Features
**Convert API calls from any source to Test-Case** : Keploy captures all the API calls and subsequent network traffic served by the application. You can use any existing API management tools like Postman, Hoppscotch, Curl to generate test-case.

* **Automatically Mocks Dependencies**
* **Safely Replays all CRUD operations**

<img src="https://github.com/keploy/docs/blob/main/static/gif/record-testcase.gif?raw=true" width="100%"  alt="Generate Test Case from API call"/>

**Native interoperability** with popular testing libraries like `go-test`. Code coverage will be reported with existing and Keploy recorded test cases and can also be integrated in CI pipelines/infrastructure.

<img src="https://github.com/keploy/docs/blob/main/static/gif/unit-test.gif?raw=true" width="100%"  alt="Generate Test Case from API call"/>

## Other Features

* **Accurate Noise Detection** in responses like (timestamps, random values) to ensure high quality tests.
* **Statistical deduplication** ensures that redundant testcases are not generated. WIP (ref [#27](https://github.com/keploy/keploy/issues/27)).
* **Web Console** to visually understand the results, update behaviour and share findings across your team.

## How it works?

![How it works](https://raw.githubusercontent.com/keploy/docs/main/static/img/how-it-works.png)

Keploy is added as a middleware to your application that captures and replays all network interaction served to application from any source. 

[Read more in detail](https://docs.keploy.io/docs/keploy-explained/how-keploy-works)


## Installation
### Start keploy server
```shell
git clone https://github.com/keploy/keploy.git && cd keploy
docker-compose up
```
The UI can be accessed at http://localhost:8081

### Helm chart
Keploy can also be installed to your your Kubernetes cluster using the Helm chart available [here](deployment/keploy)

### Run Sample application
Demos using Echo/PostgreSQL and Gin/MongoDB are available [here](https://github.com/keploy/samples-go). For this example, we will use the Echo/PostgreSQL sample.

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
go run handler.go main.go
```

### Generate testcases
To genereate testcases we just need to make some API calls. You can use [Postman](https://www.postman.com/), [Hoppscotch](https://hoppscotch.io/), or simply `curl`

#### 1. Generate shortned url
```shell
curl --request POST \
  --url http://localhost:8080/url \
  --header 'content-type: application/json' \
  --data '{
  "url": "https://github.com"
}'
```
this will return the shortened url. The ts would automatically be ignored during testing because it'll always be different.
```json
{
	"ts": 1647802058801841100,
	"url": "http://localhost:8080/GuwHCgoQ"
}
```
#### 2. Redirect to original url from shortened url
```bash
curl --request GET \
  --url http://localhost:8080/GuwHCgoQ
```

### Integration with native Go test framework
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

### Run the testcases
**Note: Before running tests stop the sample application**
```shell
go test -coverpkg=./... -covermode=atomic  ./...
```
this should show you have 74.4% coverage without writing any code!
```shell
ok      echo-psql-url-shortener 5.820s  coverage: 74.4% of statements in ./...
```

All of these can be visualised here - http://localhost:8081/testlist

## Language Support
- [x] [Go SDK](https://github.com/keploy/go-sdk)
- [ ] Java SDK - WIP [#51](https://github.com/keploy/keploy/issues/51)
- [ ] Typescript/Javascript SDK - WIP [#61](https://github.com/keploy/keploy/issues/61)
- [ ] Python SDK - WIP [#58](https://github.com/keploy/keploy/issues/58)

Need another language support? Please raise an [issue](https://github.com/keploy/keploy/issues/new?assignees=&labels=&template=feature_request.md&title=) or discuss on our [slack channel](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA)

## Resources
🤔 [FAQs](https://docs.keploy.io/docs/keploy-explained/faq)

🕵️‍️ [Why Keploy](https://docs.keploy.io/docs/keploy-explained/why-keploy)

⚙️ [Installation Guide](https://docs.keploy.io/docs/server/introduction)

📖 [Contribution Guide](https://docs.keploy.io/docs/devtools/server-contrib-guide/)


## Community Channels
We'd love to collaborate with you to make Keploy great. To get started:
* [Slack](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA) - Discussions with the community and the team.
* [GitHub](https://github.com/keploy/keploy/issues) - For bug reports and feature requests.

[![Generic badge](https://img.shields.io/badge/Slack-teal.svg?style=for-the-badge)](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA)
[![Generic badge](https://img.shields.io/badge/LinkedIn-blue.svg?style=for-the-badge)](https://www.linkedin.com/company/keploy/)
[![Generic badge](https://img.shields.io/badge/Youtube-teal.svg?style=for-the-badge)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![Generic badge](https://img.shields.io/badge/Twitter-blue.svg?style=for-the-badge)](https://twitter.com/Keployio)

Thanks goes to these wonderful people ([emoji key](https://allcontributors.org/docs/en/emoji-key)): 
<!-- ALL-CONTRIBUTORS-LIST:START - Do not remove or modify this section -->
<!-- prettier-ignore-start -->
<!-- markdownlint-disable -->
<table>
  <tr>

### Prod

<a href="https://github.com/keploy/contributors-img/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=keploy/contributors-img" />
</a>

### Staging

<a href="https://github.com/keploy/contributors-img/graphs/contributors">
  <img src="https://stg.contrib.rocks/image?repo=keploy/contributors-img" />
</a>

</tr>
</table>

<!-- markdownlint-restore -->
<!-- prettier-ignore-end -->
<!-- ALL-CONTRIBUTORS-LIST:END -->

   
