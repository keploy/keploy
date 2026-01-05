<p align="center">
  <img src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="80" alt="Keploy Logo" />
</p>

<p align="center">
 <a href="https://trendshift.io/repositories/3262" target="_blank">
    <img src="https://trendshift.io/api/badge/repositories/3262" alt="keploy%2Fkeploy | Trendshift" style="width: 250px; height: 55px;" width="250" height="55"/>
  </a>
</p>

<h3 align="center"><b>⚡️ API tests faster than unit tests, from user traffic ⚡️</b></h3>
<p align="center">🌟 The must-have tool for developers in the AI-Gen era for 90% test coverage 🌟</p>


---

<p align="center">
<a href="https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg"><img src="https://img.shields.io/badge/Slack-4A154B?style=flat&logo=slack&logoColor=white" alt="Slack" /></a>
  <a href="https://www.linkedin.com/company/keploy/"><img src="https://img.shields.io/badge/LinkedIn-%230077B5.svg?style=flat&logo=linkedin&logoColor=white" alt="LinkedIn" /></a>
  <a href="https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg"><img src="https://img.shields.io/badge/YouTube-%23FF0000.svg?style=flat&logo=YouTube&logoColor=white" alt="YouTube" /></a>
  <a href="https://x.com/Keployio"><img src="https://img.shields.io/badge/X-%231DA1F2.svg?style=flat&logo=X&logoColor=white" alt="X" /></a>
</p>

<p align="center">
  <a href="https://landscape.cncf.io/?item=app-definition-and-development--continuous-integration-delivery--keploy">
    <img src="https://img.shields.io/badge/CNCF%20Landscape-5699C6?logo=cncf&style=social" alt="Keploy CNCF Landscape" />
  </a>
<a href="https://github.com/Keploy/Keploy/stargazers"><img src="https://img.shields.io/github/stars/keploy/keploy?color=%23EAC54F&logo=github" alt="GitHub Stars" /></a>

  <a href="https://github.com/Keploy/Keploy/">
    <img src="https://img.shields.io/github/stars/keploy/keploy?color=%23EAC54F&logo=github&label=Help%20us%20reach%2020K%20stars!%20Now%20at:" alt="Help us reach 20k stars!" />
  </a>
</p>


[Keploy](https://keploy.io) is a **developer‑centric API and integration testing tool** that auto‑generates **tests and data‑mocks** faster than unit tests.  

It records API calls, database queries, and streaming events — then replays them as tests. Under the hood, Keploy **uses eBPF to capture traffic at the network layer,** but for you it’s completely **code‑less** and **language‑agnostic**.


<img align="center" src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="Convert API calls to API tests test cases and Data Mocks using AI"/>

> 🐰 **Fun fact:** Keploy uses itself for testing! Check out our swanky coverage badge: [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

---

# Key Highlights

## 🎯 No code changes

Just run your app with `keploy record`. Real API + integration flows are automatically captured as tests and mocks. *(Keploy uses eBPF under the hood to capture traffic, so you **don’t need** to add any SDKs or modify code.)* 

## 📹 Record and Replay complex Flows
Keploy can record and replay complex, distributed API flows as mocks and stubs.  It's like having a very light-weight time machine for your tests—saving you tons of time!

👉 [Read the docs on record-replay](https://keploy.io/docs/keploy-explained/introduction/)

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="Convert API calls to test cases"/>

## 🐇 Complete Infra‑Virtualization (beyond HTTP mocks)

Unlike tools that only mock HTTP endpoints, Keploy records **databases** (Postgres, MySQL, MongoDB), **streaming/queues** (Kafka, RabbitMQ), external APIs, and more. 

It replays them deterministically so you can run tests without re‑provisioning infra.

👉 [Read the docs on infra virtualisation](https://keploy.io/docs/keploy-explained/how-keploy-works/)

<img src="https://keploy-devrel.s3.us-west-2.amazonaws.com/Group+1261152745.png" width="100%" alt="Convert API calls to test cases"/>

## 🧪 Combined Test Coverage

If you’re a **developer**, you probably care about *statement* and *branch* coverage — Keploy calculates that for you. 

If you’re a **QA**, you focus more on *API schema* and *business use‑case coverage* — Keploy calculates that too. This way coverage isn’t subjective anymore. 

👉 [Read the docs on coverage](https://keploy.io/docs/server/sdk-installation/go/)

<img src="https://keploy-devrel.s3.us-west-2.amazonaws.com/keploy+ai+test+gen+for+api+statement+schema+and+branch+coverage.jpg" width="100%" alt="ai test gen for api statement schema and branch coverage"/>

## 🤖 Expand API Coverage using AI

Keploy uses existing recordings, Swagger/OpenAPI Schema to find: boundary values, missing/extra fields, wrong types, out‑of‑order sequences, retries/timeouts. 

This helps expand API Schema, Statement, and Branch Coverage. 

👉 [Read the docs on coverage](https://app.keploy.io/)

<img src="https://keploy-devrel.s3.us-west-2.amazonaws.com/ai+test+case+generation+that+works.png" width="100%" alt="ai test gen for api statement schema and branch coverage"/>


### Other Capabilities

- 🌐 **CI/CD Integration:** Run tests with mocks anywhere you like—locally on the CLI, in your CI pipeline (Jenkins, Github Actions..) , or even across a Kubernetes cluster. [Read more](https://keploy.io/docs/running-keploy/api-testing-cicd/)

- 🎭 **Multi-Purpose Mocks**: You can also use Keploy-generated Mocks, as server Tests!

- 📊 **Reporting:** Unified reports for API, integration, unit, and e2e coverage with insights directly in your CI or PRs.
- 🖥️ **Console:** A developer-friendly console to view, manage, and debug recorded tests and mocks.
- ⏱️ **Time Freezing:** Deterministically replay tests by freezing system time during execution. [Read more](https://keploy.io/docs/keploy-cloud/time-freezing/)
- 📚 **Mock Registry:** Centralized registry to manage, reuse, and version mocks across teams and environments. [Read more](https://keploy.io/docs/keploy-cloud/mock-registry/)

---

## Quick Start

### 1. Install Keploy Agent

```bash
curl --silent -O -L https://keploy.io/install.sh && source install.sh
```

### 2. Record Test Cases

Start your app under Keploy to convert real API calls into tests and mocks.

```bash
keploy record -c "CMD_TO_RUN_APP"
```

Example for Python:

```bash
keploy record -c "python main.py"
```

### 3. Run Tests

Run tests offline without external dependencies.

```bash
keploy test -c "CMD_TO_RUN_APP" --delay 10
```

## Resources
### - 📘 [Installation](https://keploy.io/docs/server/installation/)
### - 🏁 [QuickStarts](https://keploy.io/docs/quickstart/quickstart-filter/)

---

## 🛠️ Quick Start (For Contributors)

### 🧬 Prerequisites

Before you begin, ensure you have the following installed:

- **Go 1.18+** - Required for building Keploy from source
- **Docker** (optional) - For containerized testing environments
- **Git** - For cloning the repository

### 📦 Install & Run Keploy

Follow these steps to build and run Keploy from source:

#### Step 1: Clone the Repository

```bash
git clone https://github.com/keploy/keploy.git
cd keploy
```

#### Step 2: Build Keploy

```bash
make build
```

This will compile Keploy and create the executable binary in the current directory.

#### Step 3: Run Keploy

Start recording test cases with your application:

```bash
./keploy record -c "your-app-command"
```

**Example:**

```bash
# For a Python application
./keploy record -c "python main.py"

# For a Node.js application
./keploy record -c "npm start"

# For a Go application
./keploy record -c "./your-binary"
```

> 💡 **Tip:** Replace `your-app-command` with the actual command you use to run your application. Keploy will automatically capture API calls, database queries, and other network interactions.


---


## Languages &amp; Frameworks (Any stack)

Because Keploy intercepts at the **network layer (eBPF)**, it works with **any language, framework, or runtime**—no SDK required. 
> Note: Some of the dependencies are not open-source by nature because their protocols and parsings are not open-sourced. It's not supported in Keploy enterprise. 

<p align="center">

<!-- Languages -->
<img src="https://img.shields.io/badge/Go-00ADD8?logo=go&amp;logoColor=white" />
<img src="https://img.shields.io/badge/Java-ED8B00?logo=openjdk&amp;logoColor=white" />
<img src="https://img.shields.io/badge/Node.js-43853D?logo=node.js&amp;logoColor=white" />
<img src="https://img.shields.io/badge/Python-3776AB?logo=python&amp;logoColor=white" />
<img src="https://img.shields.io/badge/Rust-000000?logo=rust&amp;logoColor=white" />
<img src="https://img.shields.io/badge/C%23-239120?logo=csharp&amp;logoColor=white" />
<img src="https://img.shields.io/badge/C/C++-00599C?logo=cplusplus&amp;logoColor=white" />
<img src="https://img.shields.io/badge/TypeScript-3178C6?logo=typescript&amp;logoColor=white" />
<img src="https://img.shields.io/badge/Scala-DC322F?logo=scala&amp;logoColor=white" />
<img src="https://img.shields.io/badge/Kotlin-7F52FF?logo=kotlin&amp;logoColor=white" />
<img src="https://img.shields.io/badge/Swift-FA7343?logo=swift&amp;logoColor=white" />
<img src="https://img.shields.io/badge/Dart-0175C2?logo=dart&amp;logoColor=white" />
<img src="https://img.shields.io/badge/PHP-777BB4?logo=php&amp;logoColor=white" />
<img src="https://img.shields.io/badge/Ruby-CC342D?logo=ruby&amp;logoColor=white" />
<img src="https://img.shields.io/badge/Elixir-4B275F?logo=elixir&amp;logoColor=white" />
<img src="https://img.shields.io/badge/.NET-512BD4?logo=dotnet&amp;logoColor=white" />

<!-- Protocols &amp; infra commonly virtualized -->
<img src="https://img.shields.io/badge/gRPC-5E35B1?logo=grpc&amp;logoColor=white" />
<img src="https://img.shields.io/badge/GraphQL-E10098?logo=graphql&amp;logoColor=white" />
<img src="https://img.shields.io/badge/HTTP%2FREST-0A84FF?logo=httpie&amp;logoColor=white" />
<img src="https://img.shields.io/badge/Kafka-231F20?logo=apachekafka&amp;logoColor=white" />
<img src="https://img.shields.io/badge/RabbitMQ-FF6600?logo=rabbitmq&amp;logoColor=white" />
<img src="https://img.shields.io/badge/PostgreSQL-4169E1?logo=postgresql&amp;logoColor=white" />
<img src="https://img.shields.io/badge/MySQL-4479A1?logo=mysql&amp;logoColor=white" />
<img src="https://img.shields.io/badge/MongoDB-47A248?logo=mongodb&amp;logoColor=white" />
<img src="https://img.shields.io/badge/Redis-DC382D?logo=redis&amp;logoColor=white" />
</p>

---

## Questions? 

### Book a Live Demo / Enterprise Support

Want a guided walkthrough, dedicated support, or help planning enterprise rollout?

<p>
  <a href="https://calendar.app.google/4ZKd1nz9A5wLuP4W7">
    <img src="https://img.shields.io/badge/Request%20a%20Demo-Email-2ea44f?logo=gmail" />
  </a>
  &nbsp;
  <a href="https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg">
    <img src="https://img.shields.io/badge/Chat%20with%20Us-Slack-4A154B?logo=slack&amp;logoColor=white" />
  </a>
  <!-- Optional: replace with your scheduling link (Cal.com/Calendly) -->
  <!-- <a href="https://cal.com/keploy/demo"><img src="https://img.shields.io/badge/Book%20via%20Calendar-Cal.com-111111" /></a> -->
</p>

Prefer a calendar invite? Mention your availability in the email—we’ll send a **calendar invite** right away.

---

## Documentation & Community

- 📘 [Documentation](https://keploy.io/docs/) — Explore the full docs
- 💬 [Slack Community](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg) — Join the conversation
- 📜 [Contribution Guidelines](https://keploy.io/docs/keploy-explained/contribution-guide/)
- ❤️ [Code of Conduct](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)
- 📢 [Blog](https://keploy.io/blog/) — Read articles and updates

---

## Contribute & Collaborate

Whether you're new or experienced, your input matters. Help us improve Keploy by contributing code, reporting issues, or sharing feedback.

Together, let's build better testing tools for modern applications.
