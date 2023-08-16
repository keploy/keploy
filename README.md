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

Merge KTests with unit testing libraries(like Go-Test, JUnit..) to track combined test-coverage.

KMocks can also be referenced in existing tests or use anywhere (including any testing framework). KMocks can also be used as tests for the server.   

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/how-keploy-works.gif" width="100%"  alt="Generate Test Case from API call"/>

> Keploy is testing itself with &nbsp;  [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;  without writing many test-cases or data-mocks. 😎

[//]: # (<a href="https://www.youtube.com/watch?v=i7OqSVHjY1k"><img alt="link-to-video-demo" src="https://raw.githubusercontent.com/keploy/docs/main/static/img/link-to-demo-video.png" title="Link to Demo Video" width="50%" heigth="50%"/></a>)

## Language Support
![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)
![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54)


## How it works?
#### Safely replays all CRUD operations (including non-idempotent APIs)

Keploy act as a proxy in your application that captures and replays all network interaction served to application from any source.

Visit [How Keploy Works ?](https://docs.keploy.io/docs/keploy-explained/how-keploy-works) to read more in detail.


<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="80%"  alt="Generate Test Case from API call"/>

## Documentation

#### Here you can find the complete [Documentation](https://docs.keploy.io/) which you can refer.

## Contributing
Whether you are a community member or not, we would love your point of view! Feel free to first check out our:-

- [Contribution Guidelines](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md) - The guide outlines the process for **creating an issue** and **submitting a pull request.**
- [Code of Conduct](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md) - By following the guide we've set, your contribution will more likely be accepted if it enhances the project.

## Features

### 1. Export tests and mocks and maintain alongside existing tests

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="90%"  alt="Generate Test Case from API call"/>

### 2.  Integrates with existing Unit testing frameworks
Keploy has native interoperability as it integrates with popular testing libraries like `go-test`, `junit`. 
Code coverage will be reported with existing plus KTests. It'll also be **integrated in CI pipelines/infrastructure automatically** if you already have `go-test`, `junit` integrated.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/replay-tc.gif" width="90%"  alt="Generate Test Case from API call"/>

### 3. Accurate Noise Detection
Filters noisy fields in API responses like (timestamps, random values) to ensure high quality tests. 

### 4. Statistical De-duplication 
Ensures that redundant testcases are not generated.

# Quick Installation

<img src="https://cdn4.iconfinder.com/data/icons/logos-and-brands/512/97_Docker_logo_logos-512.png" width="20" height="20"> Docker </img>
-

Keploy can be used on <img src="https://th.bing.com/th/id/R.7802b52b7916c00014450891496fe04a?rik=r8GZM4o2Ch1tHQ&riu=http%3a%2f%2f1000logos.net%2fwp-content%2fuploads%2f2017%2f03%2fLINUX-LOGO.png&ehk=5m0lBvAd%2bzhvGg%2fu4i3%2f4EEHhF4N0PuzR%2fBmC1lFzfw%3d&risl=&pid=ImgRaw&r=0" width="10" height="10"> Linux</img> & <img src="https://cdn.freebiesupply.com/logos/large/2x/microsoft-windows-22-logo-png-transparent.png" width="10" height="10"> Windows</img> through Docker. 

> Support for MacOS is work in progress.


### Creating Alias

We need to create a custom network for Keploy since we are using the Docker.  

```zsh
docker network create keploy-network
```

Once the Custom Network is created, now we have to create the alias for the Keploy.

```shell
alias keploy='sudo docker run --name keploy-v2 -p 16789:16789 --network keploy-network --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm ghcr.io/keploy/keploy'
```

### Capture the Testcases

Now, we will use the newly created Alias `keploy` to record the testcases. Run the following command in root directory of the you're application.

```shell
keploy record -c "Docker_CMD_to_run_user_container --network keploy-network" --containerName "<contianerName>"
```

Make API Calls using [Hoppscotch](https://hoppscotch.io/), [Postman](https://www.postman.com/) or cURL command.

Keploy will capture the API calls you had made to generate the test-suites which will contain the testcases and data mocks into `YAML` format.

### Run the Testcases

Now, we will use the newly created Alias `keployV2` to test the testcases.Run the following command in root directory of the you're application.

```shell
keploy test -c "Docker_CMD_to_run_user_container --network keploy-network" --containerName "<contianerName>" --delay 20
```

> **Docker_CMD_to_run_user_container** is the docker command to run the application.

> If you are using `docker-compose` command to start the application, `--containerName` is required.

Voilà! 🧑🏻‍💻 We have the server running!

 Using **Binary** (<img src="https://th.bing.com/th/id/R.7802b52b7916c00014450891496fe04a?rik=r8GZM4o2Ch1tHQ&riu=http%3a%2f%2f1000logos.net%2fwp-content%2fuploads%2f2017%2f03%2fLINUX-LOGO.png&ehk=5m0lBvAd%2bzhvGg%2fu4i3%2f4EEHhF4N0PuzR%2fBmC1lFzfw%3d&risl=&pid=ImgRaw&r=0" width="20" height="20"> Linux</img> / <img src="https://cdn.freebiesupply.com/logos/large/2x/microsoft-windows-22-logo-png-transparent.png" width="20" height="20"> Windows</img>)
-

Keploy can be used on Linux and Windows natively. Support for `MacOS` using Colima is work in progress.

<details>
<summary>Guide for native Installation</summary>

> **Note**:- In Windows, **WSL** should be present on the system.

### Download the Keploy Binary.
```zsh
docker pull ghcr.io/keploy/keploy
```
### Capture the Testcases. 
Run this command on your terminal to start the recording of API calls:-

```zsh
sudo -E keploy record -c "CMD_TO_RUN_APP"
```
for example, if you are golang framework then the command would be:-

```zsh
sudo -E keploy record -c "relative_path_of_go_binary of your application"
```
### Run the Testcases.
Run this command on your terminal to run the testcases and generate the test coverage report:-

```zsh
sudo -E keploy test -c "CMD_TO_RUN_APP" --delay 10
```

for example, if you are golang framework then the command would be:-

```zsh
sudo -E keploy test -c "relative_path_of_go_binary of your application" --delay 10
```
</details>

## Keploy Modes
There are 2 Keploy modes:

1. **Record mode** :
	* Record requests, response and all external calls and sends to Keploy server.
	* After keploy server removes duplicates, it then runs the request on the API again to identify noisy fields.
	* Sends the noisy fields to the keploy server to be saved along with the testcase.


2. **Test mode** :
	* Fetches testcases for the app from keploy server.
	* Calls the API with same request payload in testcase.
	* Mocks external calls based on data stored in the testcase.
	* Validates the responses and uploads results to the keploy server

##  Current Limitations
* **Unit Testing**: While Keploy is designed to run alongside unit testing frameworks (Go test, JUnit..) and can add to the overall code coverage, it still generates E2E tests. 

* **Production usage** Keploy is currently focused on generating tests for developers. These tests can be captured from any environment, but we have not tested it on high volume production environments. This would need robust deduplication to avoid too many redundant tests being captured. We do have ideas on building a robust deduplication system [#27](https://github.com/keploy/keploy/issues/27) 

## Resources
🤔 [FAQs](https://docs.keploy.io/docs/keploy-explained/faq)

🕵️‍️ [Why Keploy](https://docs.keploy.io/docs/keploy-explained/why-keploy)

⚙️ [Installation Guide](https://docs.keploy.io/docs/server/server-installation)

📖 [Contribution Guide](https://docs.keploy.io/docs/devtools/server-contrib-guide/)


## Community Support  ❤️

We'd love to collaborate with you to make Keploy great. To get started:

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![Twitter](https://img.shields.io/badge/Twitter-%231DA1F2.svg?style=for-the-badge&logo=Twitter&logoColor=white)](https://twitter.com/Keployio)

<!-- markdownlint-restore -->
<!-- prettier-ignore-end -->
<!-- ALL-CONTRIBUTORS-LIST:END -->
