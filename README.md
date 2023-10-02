
<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="keploy logo"/>
</p>
<h3 align="center">
<b>
⚡️ Backend tests faster than unit-tests, from user traffic ⚡️
</b>
</h3 >
<p align="center">
🌟 The must-have tool for developers in the AI-Gen era 🌟
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
    <img src=".github/docs.svg" /></a></p>

## 🎤 Introducing Keploy 🐰
Keploy is a **developer-centric** backend testing tool. It makes **backend tests with built-in-mocks**, faster than unit tests, from user traffic, making it **easy-to-use, powerful and extensible**. 🛠

Ready for the magic? Here are Keploy's core features:

- ♻️ **Combined Test Coverage:** Merge your Keploy Tests with your fave testing libraries(junit, go-test, py-test, jest) to see a combined test-coverage.


- 🤖 **EBPF Instrumentation:** Keploy uses EBPF like a secret sauce to make integration code-less, language agnostic, and oh-so-lightweight.


- 🌐 **CI/CD Integration:** Run tests with mocks anywhere you like—locally on the CLI, in your CI pipeline, or even across a Kubernetes cluster. It's testing wherever you want it!


- 🎭 **Multi-Purpose Mocks:** Use 'em in existing tests, as server tests, or just to impress your friends!


- 📽️ **Record-Replay Complex Flows:** Keploy can record and replay complex, distributed API flows as mocks and stubs. It's like having a time machine for your tests—saving you tons of time!


<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/how-keploy-works.gif" width="70%" alt="Generate Test Case from API call"/>


> 🐰 **Fun fact:** Keploy uses itself for testing! Check out our swanky coverage badge: [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

## 🌐 Language Support
From Go's gopher 🐹 to Python's snake 🐍, we support:

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)
![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54)

## 🎩 How's the Magic Happen?
Our magical 🧙‍♂️ Keploy proxy captures and replays **ALL**(CRUD operations, including non-idempotent APIs) your app's network interactions.


Take a journey to **[How Keploy Works?](https://docs.keploy.io/docs/keploy-explained/how-keploy-works)** to discover the tricks behind the curtain!

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="50%" alt="Generate Test Case from API call"/>

## 📘 Get Schooled!
Become a Keploy pro with our **[Documentation](https://docs.keploy.io/)**.

# Quick Installation

Using **Binary** (<img src="https://th.bing.com/th/id/R.7802b52b7916c00014450891496fe04a?rik=r8GZM4o2Ch1tHQ&riu=http%3a%2f%2f1000logos.net%2fwp-content%2fuploads%2f2017%2f03%2fLINUX-LOGO.png&ehk=5m0lBvAd%2bzhvGg%2fu4i3%2f4EEHhF4N0PuzR%2fBmC1lFzfw%3d&risl=&pid=ImgRaw&r=0" width="20" height="20"> Linux</img> / <img src="https://cdn.freebiesupply.com/logos/large/2x/microsoft-windows-22-logo-png-transparent.png" width="20" height="20"> WSL</img>)
-

Keploy can be utilized on Linux natively and through WSL on Windows.

### Download the Keploy Binary.


```zsh
curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz" | tar xz -C /tmp

sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin && keploy
```

<details>
 <summary> <strong> ARM Architecture </strong> </summary>

```zsh
curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz" | tar xz -C /tmp

sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin && keploy
```

</details>

### Capturing Testcases
To initiate the recording of API calls, execute this command in your terminal where you usually run your app, if you
have to set any environment variables do it like you usually do it:

```zsh
sudo -E env PATH=$PATH keploy record -c "CMD_TO_RUN_APP"
```
For instance, if you're using a simple Golang program, the command would resemble:

```zsh
sudo -E env PATH=$PATH keploy record -c "go run main.go"
```

### Running Testcases
To run the testcases and generate a test coverage report, use this terminal command where you usually run your app, if
you have to set any environment variables do it like you usually do it:

```zsh
sudo -E env PATH=$PATH keploy test -c "CMD_TO_RUN_APP" --delay 10
```

For example, if you're using a Golang framework, the command would be:

```zsh
sudo -E env PATH=$PATH keploy test -c "go run main.go" --delay 10
```

<img src="https://cdn4.iconfinder.com/data/icons/logos-and-brands/512/97_Docker_logo_logos-512.png" width="20" height="20"> Docker Installation </img>
-

Keploy can be used on <img src="https://th.bing.com/th/id/R.7802b52b7916c00014450891496fe04a?rik=r8GZM4o2Ch1tHQ&riu=http%3a%2f%2f1000logos.net%2fwp-content%2fuploads%2f2017%2f03%2fLINUX-LOGO.png&ehk=5m0lBvAd%2bzhvGg%2fu4i3%2f4EEHhF4N0PuzR%2fBmC1lFzfw%3d&risl=&pid=ImgRaw&r=0" width="10" height="10"> Linux</img> & <img src="https://cdn.freebiesupply.com/logos/large/2x/microsoft-windows-22-logo-png-transparent.png" width="10" height="10"> Windows</img> through [Docker](https://docs.docker.com/engine/install).

> **️ Note:** <img src="https://www.pngplay.com/wp-content/uploads/3/Apple-Logo-Transparent-Images.png" width="15" height="15"> MacOS</img> users need to install [Colima](https://github.com/abiosoft/colima#installation). <img src="https://cdn.freebiesupply.com/logos/large/2x/microsoft-windows-22-logo-png-transparent.png" width="10" height="10"/> Windows users need to install [WSL](https://learn.microsoft.com/en-us/windows/wsl/install#install-wsl-command).


### Creating alias

Let's create an alias for Keploy:

```shell
alias keploy='sudo docker run --pull always --name keploy-v2 -p 16789:16789 --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v '"$HOME"'/.keploy:'"$HOME"'/.keploy --rm ghcr.io/keploy/keploy'
```

### Recording Testcases and Data Mocks

Here are few points to consider before recording!
- If you're running via **docker compose**, ensure to include the `<CONTAINER_NAME>` under your application service in the docker-compose.yaml file [like this](https://github.com/keploy/samples-python/blob/9d6cf40da2eb75f6e035bedfb30e54564785d5c9/flask-mongo/docker-compose.yml#L14).
- You must run the containers on a network, if not, ensure all your containers are on same network with external property true - [like this](https://github.com/keploy/samples-python/blob/9d6cf40da2eb75f6e035bedfb30e54564785d5c9/flask-mongo/docker-compose.yml#L24). Replace the **network name** (`--network` flag) to your custom network if you changed it above, like <backend> network in given example.
- `Docker_CMD_to_run_user_container` refers to the Docker **command for launching** the application.

Utilize the keploy alias we created to capture testcases. **Execute** the following command within your application's **root directory**.

```shell
keploy record -c "Docker_CMD_to_run_user_container --network <network_name>" --containerName "<container_name>"
```
Perform API calls using tools like [Hoppscotch](https://hoppscotch.io/), [Postman](https://www.postman.com/), or cURL commands.

Keploy will capture the API calls you've conducted, generating test suites comprising **testcases (KTests) and data mocks (KMocks)** in `YAML` format.

### Running Testcases

Now, use the keployV2 Alias we created to execute the testcases. Follow these steps in the **root directory** of your application.

When using **docker-compose** to start the application, it's important to ensure that the `--containerName` parameter matches the container name in your `docker-compose.yaml` file.


```shell
keploy test -c "Docker_CMD_to_run_user_container --network <network_name>" --containerName "<container_name>" --delay 20
```

Voilà! 🧑🏻‍💻 We have the tests with data mocks running! 🐰🎉

You'll be able to see the test-cases that ran with the results report on the console as well locally in the `testReport` directory.

## 🤔 Questions?
Reach out to us. We're here to help!

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![Twitter](https://img.shields.io/badge/Twitter-%231DA1F2.svg?style=for-the-badge&logo=Twitter&logoColor=white)](https://twitter.com/Keployio)

## 💖 Let's Build Together!
Whether you're a newbie coder or a wizard 🧙‍♀️, your perspective is golden. Take a peek at our:

📜 [Contribution Guidelines](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md)

❤️ [Code of Conduct](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)


## 🌟 Features

### **🚀 Export, maintain, and show off tests and mocks!**

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="90%"  alt="Generate Test Case from API call"/>

### **🤝 Shake hands with popular testing frameworks – Go-Test, JUnit, Py-Test, Jest and more!**

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/replay-tc.gif" width="90%"  alt="Generate Test Case from API call"/>

### **🕵️ Detect noise with surgeon-like precision!**
Filters noisy fields in API responses like (timestamps, random values) to ensure high quality tests.

### **📊 Say 'Hello' to higher coverage!**
Keploy ensures that redundant testcases are not generated.


## 🐲 The Challenges We Face!
- **Unit Testing:** While Keploy is designed to run alongside unit testing frameworks (Go test, JUnit..) and can add to the overall code coverage, it still generates E2E tests.
- **Production Lands**: Keploy is currently focused on generating tests for developers. These tests can be captured from any environment, but we have not tested it on high volume production environments. This would need robust deduplication to avoid too many redundant tests being captured. We do have ideas on building a robust deduplication system [#27](https://github.com/keploy/keploy/issues/27)

## ✨ Resources!
🤔 [FAQs](https://docs.keploy.io/docs/keploy-explained/faq)

🕵️‍️ [Why Keploy](https://docs.keploy.io/docs/keploy-explained/why-keploy)

⚙️ [Installation Guide](https://docs.keploy.io/docs/server/server-installation)

📖 [Contribution Guide](https://docs.keploy.io/docs/devtools/server-contrib-guide/)


## 🌟 Hall of Contributors
<p>
  <img src="https://api.vaunt.dev/v1/github/entities/keploy/repositories/keploy/contributors?format=svg&limit=18" width="100%"  alt="contributors"/>
</p>

### Available Awards

| Name | Icon | Description |
| ---- | ---- | ----------- |
| Docs Maker | <img src="https://raw.githubusercontent.com/sonichigo/keploy/main/.vaunt/badge/docs_hero.png" width="150" /> | Awarded for helping in improving Keploy documentation! |
| Every Bit Counts | <img src="https://raw.githubusercontent.com/sonichigo/keploy/main/.vaunt/badge/commit_hero.png" width="150" /> | No commit is too small! |
| Pull Request Hero | <img src="https://raw.githubusercontent.com/sonichigo/keploy/main/.vaunt/badge/pull_request_hero.png" width="150" /> | You're a PR hero, rock on! |
| Closer| <img src="https://raw.githubusercontent.com/sonichigo/keploy/main/.vaunt/badge/closer.png" width="150" /> | Only closers get coffee! |
