
<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="keploy logo"/>
</p>
<h3 align="center">
<b>
⚡️ API tests faster than unit tests, from user traffic ⚡️
</b>
</h3 >
<p align="center">
🌟 The must-have tool for developers in the AI-Gen era 🌟
</p>

---

<h4 align="center">

   <a href="https://x.com/Keployio">
    <img src="https://img.shields.io/badge/follow-%40keployio-1DA1F2?logo=X&style=social" alt="Keploy X!" />
  </a>
  
  <a href="https://github.com/Keploy/Keploy/">
   <img src="https://img.shields.io/github/stars/keploy/keploy?color=%23EAC54F&logo=github&label=Help%20us%20reach%2020K%20stars!%20Now%20at:" alt="Help us reach 20k stars!" />
</a>

  <a href="https://landscape.cncf.io/?item=app-definition-and-development--continuous-integration-delivery--keploy">
    <img src="https://img.shields.io/badge/CNCF%20Landscape-5699C6?logo=cncf&style=social" alt="Keploy CNCF Landscape" />
  </a>

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)

<a href="https://trendshift.io/repositories/3262" target="_blank"><img src="https://trendshift.io/api/badge/repositories/3262" alt="keploy%2Fkeploy | Trendshift" style="width: 250px; height: 55px;" width="250" height="55"/></a>
</h4>


[Keploy](https://keploy.io) is **developer-centric** API testing tool that creates **tests along with built-in-mocks**, faster than unit tests.

Keploy not only records API calls, but also records database calls and replays them during testing, making it **easy to use, powerful, and extensible**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="Convert API calls to test cases"/>

> 🐰 **Fun fact:** Keploy uses itself for testing! Check out our swanky coverage badge: [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

## 🚨 Here for  [Unit Test Generator](README-UnitGen.md) (ut-gen)? 
Keploy has newly launched the world's first unit test generator(ut-gen) implementation of [Meta LLM research paper](https://arxiv.org/pdf/2402.09171), it understands code semantics and generates meaningful unit tests, aiming to:

- **Automate unit test generation (UTG)**: Quickly generate comprehensive unit tests and reduce redundant manual effort.

- **Improve edge cases**: Extend and improve the scope of automated tests to cover more complex scenarios, often missed manually.

- **Boost test coverage**: As codebases grow, ensuring exhaustive coverage should become feasible, aligning with our mission.

### 📜 Follow [Unit Test Generator README](README-UnitGen.md)! ✅

## 📘 Documentation!
Become a Keploy pro with **[Keploy Documentation](https://keploy.io/docs/)**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="Record Replay Testing"/>

# 🚀 Quick Installation (API test generator)

Integrate Keploy by installing the agent locally. No code-changes required.

```shell
curl --silent -O -L https://keploy.io/install.sh && source install.sh
```

##  🎬 Recording Testcases

Start your app with Keploy to convert API calls as Tests and Mocks/Stubs.

```zsh
keploy record -c "CMD_TO_RUN_APP" 
```
For example, if you're using a simple Python app the `CMD_TO_RUN_APP` would resemble to `python main.py`, for  Golang `go run main.go`, for java `java -jar xyz.jar`, for node `npm start`..

```zsh
keploy record -c "python main.py"
```

## 🧪 Running Tests
Shut down the databases, redis, kafka or any other services your application uses. Keploy doesn't need those during test.
```zsh
keploy test -c "CMD_TO_RUN_APP" --delay 10
```

## ✅ Test Coverage Integration
To integrate with your unit-testing library and see combine test coverage, follow this [test-coverage guide](https://keploy.io/docs/server/sdk-installation/go/).

> ####  **If You Had Fun:** Please leave a 🌟 star on this repo! It's free and will bring a smile. 😄 👏

## One-Click Setup 🚀

Setup and run keploy quickly, with no local machine installation required:

[![GitHub Codescape](https://img.shields.io/badge/GH%20codespace-3670A0?style=for-the-badge&logo=github&logoColor=fff)]([https://github.dev/Sonichigo/mux-sql](https://github.dev/Sonichigo/mux-sql))

## 🤔 Questions?
Reach out to us. We're here to help!

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)


## 🌐 Language Support
From Go's gopher 🐹 to Python's snake 🐍, we support:

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)
![Rust](https://img.shields.io/badge/Rust-darkred?style=for-the-badge&logo=rust&logoColor=white)
![C#](https://img.shields.io/badge/csharp-purple?style=for-the-badge&logo=csharp&logoColor=white)
![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54)

## 🫰 Keploy Adopters 🧡

So you and your organisation are using Keploy? That’s great. Please add yourselves to [**this list,**](https://github.com/orgs/keploy/discussions/1765) and we'll send you goodies! 💖


We are happy and proud to have you all as part of our community! 💖

## 🎩 How's the Magic Happen?
Keploy proxy captures and replays **ALL** (CRUD operations, including non-idempotent APIs) of your app's network interactions.


Take a journey to **[How Keploy Works?](https://keploy.io/docs/keploy-explained/how-keploy-works/)** to discover the tricks behind the curtain!

  ## 🔧 Core Features

- ♻️ **Combined Test Coverage:** Merge your Keploy Tests with your fave testing libraries(JUnit, go-test, py-test, jest) to see a combined test coverage.


- 🤖 **EBPF Instrumentation:** Keploy uses EBPF like a secret sauce to make integration code-less, language-agnostic, and oh-so-lightweight.


- 🌐 **CI/CD Integration:** Run tests with mocks anywhere you like—locally on the CLI, in your CI pipeline (Jenkins, Github Actions..) , or even across a Kubernetes cluster.


- 📽️ **Record-Replay Complex Flows:** Keploy can record and replay complex, distributed API flows as mocks and stubs. It's like having a time machine for your tests—saving you tons of time!


- 🎭 **Multi-Purpose Mocks:** You can also use Keploy-generated Mocks, as server Tests!


👉 **Explore the code on GitHub**: [github.com/keploy/keploy](https://github.com/keploy/keploy)


## 👨🏻‍💻 Let's Build Together! 👩🏻‍💻
Whether you're a newbie coder or a wizard 🧙‍♀️, your perspective is golden. Take a peek at our:

📜 [Contribution Guidelines](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md)

❤️ [Code of Conduct](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)


## 🐲 Current Limitations!
- **Unit Testing:** While Keploy is designed to run alongside unit testing frameworks (Go test, JUnit..) and can add to the overall code coverage, it still generates integration tests.
- **Production Lands**: Keploy is currently focused on generating tests for developers. These tests can be captured from any environment, but we have not tested it on high volume production environments. This would need robust deduplication to avoid too many redundant tests being captured. We do have ideas on building a robust deduplication system [#27](https://github.com/keploy/keploy/issues/27)

## ✨ Resources!
🤔 [FAQs](https://keploy.io/docs/keploy-explained/faq/)

🕵️‍️ [Why Keploy](https://keploy.io/docs/keploy-explained/why-keploy/)

⚙️ [Installation Guide](https://keploy.io/docs/application-development/)

📖 [Contribution Guide](https://keploy.io/docs/keploy-explained/contribution-guide/)
