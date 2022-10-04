# Welcome to Keploy 👋

<p style="text-align:center;" align="center">
  <img align="center" src="https://avatars.githubusercontent.com/u/92252339?s=200&v=4" height="20%" width="20%" />
</p>


[![contributions welcome](https://img.shields.io/badge/contributions-welcome-brightgreen?logo=github)](CODE_OF_CONDUCT.md) 
[![Tests](https://github.com/keploy/keploy/actions/workflows/go.yml/badge.svg)](https://github.com/keploy/keploy/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/keploy/keploy)](https://goreportcard.com/report/github.com/keploy/keploy)
[![Slack](.github/slack.svg)](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA)
[![Docs](.github/docs.svg)](https://docs.keploy.io)
[![Gitpod](https://img.shields.io/badge/Gitpod-ready--to--code-FFB45B?logo=gitpod)](https://gitpod.io/#https://github.com/keploy/samples-go) 


# Keploy
Keploy is a no-code API testing platform that generates tests-cases and data-mocks from API calls.

Dependency-mocks are automatically generated with the recorded request/responses. 

> Keploy is testing itself with &nbsp;  [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main)](https://coveralls.io/github/keploy/keploy?branch=main) &nbsp;  without writing any test-cases and data-mocks. 😎
<a href="https://www.youtube.com/watch?v=i7OqSVHjY1k"><img alt="link-to-video-demo" src="https://raw.githubusercontent.com/keploy/docs/main/static/img/link-to-demo-video.png" title="Link to Demo Video" width="50%"/></a>

## Quick Start
The fastest way to start with Keploy is the Gitpod-hosted version. When you're ready, you can install locally or host yourself.

One-click deploy sample URL Shortener application sample with Keploy using Gitpod 

[![Open in Gitpod](https://gitpod.io/button/open-in-gitpod.svg)](https://gitpod.io/#https://github.com/keploy/samples-go)

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
* **Test Export** generates and stores testcases(and their mocks) in the project directory or mongoDB cluster. By default, they are stored in project directory.

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
Keploy can also be installed to your Kubernetes cluster using the Helm chart available [here](deployment/keploy)

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
- [x] [Java SDK](https://github.com/keploy/java-sdk)
- [x] [Typescript/Javascript SDK](https://github.com/keploy/typescript-sdk)
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

## 📌 Our valuable Contributors👩‍💻👨‍💻 :

Thanks goes to these wonderful people ([emoji key](https://allcontributors.org/docs/en/emoji-key)): 
<!-- ALL-CONTRIBUTORS-LIST:START - Do not remove or modify this section -->
<!-- prettier-ignore-start -->
<!-- markdownlint-disable -->
<table>
  <tr>

<td align="center"><a href="https://github.com/slayerjain"><img src="https://avatars.githubusercontent.com/u/12831254?v=4" width="100px;" alt=""/><br /><sub><b>Shubham Jain</b></sub></a><br /><a href="#maintenance-slayerjain" title="Maintenance">🚧</a></td>
<td align="center"><a href="https://github.com/Sarthak160"><img src="https://avatars.githubusercontent.com/u/50234097?v=4" width="100px;" alt=""/><br /><sub><b>Sarthak</b></sub></a><br /><a href="contributer-Sarthak160" title="Contributer">🚧</a></td>
<td align="center"><a href="https://github.com/re-Tick"><img src="https://avatars.githubusercontent.com/u/60597329?v=4" width="100px;" alt=""/><br /><sub><b>Ritik Jain</b></sub></a><br /><a href="#contributer-slayerjain" title="Contributer">🚧</a></td>
<td align="center"><a href="https://github.com/nehagup"><img src="https://avatars.githubusercontent.com/u/15074229?v=4" width="100px;" alt=""/><br /><sub><b>Neha Gupta</b></sub></a><br /><a href="#maintenance-nehagup" title="Maintenance">🚧</a></td>
<td align="center"><a href="https://github.com/Ayush7614"><img src="https://avatars.githubusercontent.com/u/67006255?v=4" width="100px;" alt=""/><br /><sub><b>Felix-Ayush</b></sub></a><br /><a href="#contributer-Ayush7614" title="Contributer">🚧</a></td>
<td align="center"><a href="https://github.com/madhavsikka"><img src="https://avatars.githubusercontent.com/u/39848688?v=4" width="100px;" alt=""/><br /><sub><b>Madhav Sikka</b></sub></a><br /><a href="#maintenance-madhavsikka" title="Contributer">🚧</a></td>
<td align="center"><a href="https://github.com/unnati914"><img src="https://avatars.githubusercontent.com/u/69121168?v=4" width="100px;" alt=""/><br /><sub><b>Unnati</b></sub></a><br /><a href="#contributer-unnati914" title="Contributer">🚧</a></td>
<td align="center"><a href="https://github.com/thunderboltsid"><img src="https://avatars.githubusercontent.com/u/6081171?v=4" width="100px;" alt=""/><br /><sub><b>Sid Shukla</b></sub></a><br /><a href="#contributer-thunderboltsid" title="Contributer">🚧</a></td>
<td align="center"><a href="https://github.com/petergeorgas"><img src="https://avatars.githubusercontent.com/u/21143531?v=4" width="100px;" alt=""/><br /><sub><b>Peter Georgas</b></sub></a><br /><a href="#contributer-petergeorgas" title="Contributer">🚧</a></td>
<td align="center"><a href="https://github.com/michaelgrigoryan25"><img src="https://avatars.githubusercontent.com/u/56165400?v=4" width="100px;" alt=""/><br /><sub><b>Michael Grigoryan</b></sub></a><br /><a href="#contributer-michaelgrigoryan25" title="Contributer">🚧</a></td>
<td align="center"><a href="https://github.com/skant7"><img src="https://avatars.githubusercontent.com/u/65185019?v=4" width="100px;" alt=""/><br /><sub><b>Surya Kant</b></sub></a><br /><a href="#contributer-skant7" title="Contributer">🚧</a></td>
<td align="center"><a href="https://github.com/mahi-official"><img src="https://avatars.githubusercontent.com/u/25299699?v=4" width="100px;" alt=""/><br /><sub><b>Mahesh Gupta</b></sub></a><br /><a href="#contributer-mahi-official" title="Contributer">🚧</a></td>
<td align="center"><a href="https://github.com/namantaneja167"><img src="https://avatars.githubusercontent.com/u/42579074?v=4" width="100px;" alt=""/><br /><sub><b>Naman Taneja</b></sub></a><br /><a href="#contributer-namantaneja167" title="Contributer">🚧</a></td>
<td align="center"><a href="https://github.com/rajatsharma"><img src="https://avatars.githubusercontent.com/u/13231434?v=4" width="100px;" alt=""/><br /><sub><b>Rajat Sharma</b></sub></a><br /><a href="#contributer-rajatsharma" title="Contributer">🚧</a></td>
<td align="center"><a href="https://github.com/Akshit42-hue"><img src="https://avatars.githubusercontent.com/u/59443454?v=4" width="100px;" alt=""/><br /><sub><b>Axit Patel</b></sub></a><br /><a href="#contributer-Akshit42-hue" title="Contributer">🚧</a></td>
<td align="center"><a href="https://github.com/ditsuke"><img src="https://avatars.githubusercontent.com/u/72784348?v=4" width="100px;" alt=""/><br /><sub><b>Tushar Malik</b></sub></a><br /><a href="#contributer-ditsuke" title="Contributer">🚧</a></td>

</tr>
</table>

<!-- markdownlint-restore -->
<!-- prettier-ignore-end -->
<!-- ALL-CONTRIBUTORS-LIST:END -->

## Launching keploy Rewards
 Contributed to keploy? Here is a big thank you from our community to you.
 Claim your badge and showcase them with pride.
 Let us inspire more folks !

 ![keploy Badges](https://aviyel.com/assets/uploads/rewards/share/project/41/512/share.png)
 ### **[Claim Now!](https://aviyel.com/projects/41/keploy/rewards)**
