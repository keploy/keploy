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

  
[![Twitter Follow](https://img.shields.io/badge/Follow%20@Keployio-1DA1F2?style=for-the-badge&logo=twitter&logoColor=white)](https://twitter.com/Keploy_io)
[![GitHub Stars](https://img.shields.io/github/stars/keploy/keploy?style=for-the-badge&logo=github&color=EAC54F&labelColor=000000&label=Help%20us%20reach%2010K%20stars!)](https://github.com/Keploy/Keploy/issues)
[![CNCF Landscape](https://img.shields.io/badge/CNCF_Landscape-0078D4?style=for-the-badge&logo=data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAyNCAyNCI+PHBhdGggZmlsbD0id2hpdGUiIGQ9Ik0xMiAyTDIgNy4wMDl2OS45ODJMMTIgMjJsMTAtNS4wMDlWNy4wMDlMMTIgMnptMCAxNi41TDQuNSAxNHYtNEwxMiA1LjVsNy41IDQuNXY0TDEyIDE4LjV6Ii8+PC9zdmc+)](https://landscape.cncf.io/?item=app-definition-and-development--continuous-integration-delivery--keploy)



[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-2poflru6f-_VAuvQfCBT8fDWv1WwSbkw)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![Twitter](https://img.shields.io/badge/Twitter-%231DA1F2.svg?style=for-the-badge&logo=Twitter&logoColor=white)](https://twitter.com/Keployio)

<a href="https://trendshift.io/repositories/3262" target="_blank"><img src="https://trendshift.io/api/badge/repositories/3262" alt="keploy%2Fkeploy | Trendshift" style="width: 250px; height: 55px;" width="250" height="55"/></a>
</h4>


# 📜 Table of Contents  

- [🚀 Quick Installation](#-quick-installation)  
- [📘 Documentation](#-documentation)  
- [🌐 Language Support](#-language-support)  
- [🏆 Adopters](#-adopters)  
- [🛠 Contributing](#-contributing)  
- [⚠️ Current Limitations](#️-current-limitations)  
- [📚 Resources](#-resources)  
- [❓ Questions](#-questions)  
---

[Keploy](https://keploy.io) is a **developer-centric** API testing tool that simplifies and accelerates the testing process by creating **tests with built-in mocks**, making it significantly faster than traditional unit tests.

In addition to recording API calls, Keploy also captures database interactions and replays them during testing, ensuring a seamless, reliable experience. This makes Keploy **easy to use, powerful, and highly extensible**.


<!--<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="Convert API calls to test cases"/>-->

> 🐰 **Fun fact:** Keploy uses itself for testing! Check out our swanky coverage badge: [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;


<!-- ### 📜 Follow [Unit Test Generator README](README-UnitGen.md)! ✅ -->


## 🚀 Quick Installation (API test generator)


Integrate Keploy seamlessly into your development workflow with no code changes required. Let’s explore its features one by one:

---

## 🧪 Unit Test Generator Features  

Keploy’s AI-powered VS Code extension revolutionizes unit testing by automating the generation of test cases directly from your code. This not only improves test coverage but also saves you valuable time and effort. Key benefits include:  
- **Automatic Test Generation:** Keploy analyzes your code and creates unit tests for enhanced reliability.  
- **Improved Test Coverage:** Generates detailed test cases that ensure your application meets high-quality standards.  
- **Ease of Use:** No manual effort is required to write boilerplate tests.  

Elevate your unit testing game with Keploy's VS Code AI Extension today!  

### 📜 Follow [Unit Test Generator AI Extension](https://marketplace.visualstudio.com/items?itemName=Keploy.keployio)! ✅  

---

## 🔗 Integration Testing  

Keploy simplifies integration testing by capturing and replaying **ALL** your app's network interactions, including CRUD operations and non-idempotent APIs. This ensures seamless communication between application components while detecting and addressing compatibility issues early.  

### 🛠 How it Works:  
Keploy acts as a proxy that records your app's network interactions and replays them during testing to validate behavior. The magic lies in its ability to simulate real-world scenarios effortlessly!  

Take a journey to **[How Keploy Works?](https://keploy.io/docs/keploy-explained/how-keploy-works/)** to discover the tricks behind the curtain!

### Keploy’s Core Features:  
| **Feature**                  | **Description**                                                                                                             |
|------------------------------|-----------------------------------------------------------------------------------------------------------------------------|
| ♻️ **Combined Test Coverage** | Merge your Keploy Tests with your favorite testing libraries (JUnit, go-test, py-test, jest) to see a combined test coverage. |
| 🤖 **EBPF Instrumentation**   | Keploy uses EBPF like a secret sauce to make integration code-less, language-agnostic, and lightweight.                      |
| 🌐 **CI/CD Integration**      | Run tests with mocks locally, in CI pipelines (e.g., Jenkins, GitHub Actions), or across Kubernetes clusters.                |
| 📽️ **Record-Replay Flows**   | Record and replay distributed API flows as mocks/stubs, like a time machine for your tests.                                  |
| 🎭 **Multi-Purpose Mocks**    | Use Keploy Mocks as server tests.                                                                                           |

---

## 🌐 API Testing  

Keploy automates API testing by recording API requests and responses during runtime. These recordings are transformed into reusable test cases, allowing you to validate your APIs efficiently.  

### **Steps to Get Started:**  
1. **Set Up Keploy Locally:** Install Keploy with one-click installation and minimal configuration.  
2. **Capture API Traffic:** Run your application while Keploy records all API requests and responses.  
3. **Generate Test Cases:** Use the captured data to create reusable test cases with mock data and stubs for validation.  
4. **Run Tests Anywhere:** Validate API behavior locally, in CI/CD pipelines, or even across Kubernetes clusters.  

Keploy ensures consistent API behavior, improves API quality, and saves manual testing effort. 🚀  

Ready to dive deeper? Check out Keploy’s [API Testing Documentation](https://keploy.io/docs)!  


<!--
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
To integrate with your unit-testing library and see combine test coverage, follow this [test-coverage guide](https://keploy.io/docs/server/sdk-installation/go/).-->



## One-Click Setup 🚀

Setup and run keploy quickly, with no local machine installation required:

[![GitHub Codespace](https://img.shields.io/badge/GH%20codespace-3670A0?style=for-the-badge&logo=github&logoColor=fff)](https://dev.github.com/Sonichigo/mux-sql)

## 🚨 Here for  [Unit Test Generator](README-UnitGen.md) (ut-gen)? 
Keploy has newly launched the world's first unit test generator(ut-gen) implementation of [Meta LLM research paper](https://arxiv.org/pdf/2402.09171), it understands code semantics and generates meaningful unit tests, aiming to:

| **Feature**                  | **Description**                                                                 |
|------------------------------|---------------------------------------------------------------------------------|
| **Automate Unit Test Generation (UTG)** | Quickly generates comprehensive unit tests, reducing redundant manual effort.      |
| **Improve Edge Cases**       | Extends and enhances the scope of automated tests to cover complex scenarios.   |
| **Boost Test Coverage**      | Ensures exhaustive test coverage as codebases grow, aligning with Keploy's mission. |



## 📘 Documentation!
Become a Keploy pro with **[Keploy Documentation](https://keploy.io/docs/)**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="Record Replay Testing"/>


## 🌐 Language Support
From Go's gopher 🐹 to Python's snake 🐍, we support:

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)
![Rust](https://img.shields.io/badge/Rust-darkred?style=for-the-badge&logo=rust&logoColor=white)
![C#](https://img.shields.io/badge/csharp-purple?style=for-the-badge&logo=csharp&logoColor=white)
![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54)
![PHP](https://img.shields.io/badge/php-777BB4?style=for-the-badge&logo=php&logoColor=white)


## 🫰 Keploy Adopters 🧡

So you and your organisation are using Keploy? That’s great. Please add yourselves to [**this list,**](https://github.com/orgs/keploy/discussions/1765) and we'll send you goodies! 💖


We are happy and proud to have you all as part of our community! 💖

## 🎩 How's the Magic Happen?
Keploy proxy captures and replays **ALL** (CRUD operations, including non-idempotent APIs) of your app's network interactions.


Take a journey to **[How Keploy Works?](https://keploy.io/docs/keploy-explained/how-keploy-works/)** to discover the tricks behind the curtain!

Here are Keploy's core features: 🛠

| **Feature**                  | **Description**                                                                                                             |
|------------------------------|-----------------------------------------------------------------------------------------------------------------------------|
| ♻️ **Combined Test Coverage** | Merge your Keploy Tests with your favorite testing libraries (JUnit, go-test, py-test, jest) to see a combined test coverage. |
| 🤖 **EBPF Instrumentation**   | Keploy uses EBPF like a secret sauce to make integration code-less, language-agnostic, and lightweight.                      |
| 🌐 **CI/CD Integration**      | Run tests with mocks locally, in CI pipelines (e.g., Jenkins, GitHub Actions), or across Kubernetes clusters.                |
| 📽️ **Record-Replay Flows**   | Record and replay distributed API flows as mocks/stubs, like a time machine for your tests.                                  |
| 🎭 **Multi-Purpose Mocks**    | Use Keploy Mocks as server tests.  


## 👨🏻‍💻 Let's Build Together! 👩🏻‍💻
Whether you're a newbie coder or a wizard 🧙‍♀️, your perspective is golden. Take a peek at our:

📜 [Contribution Guidelines](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md)

❤️ [Code of Conduct](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)


## 🐲 Current Limitations!
- **Production Lands**: Keploy is currently focused on generating tests for developers. These tests can be captured from any environment, but we have not tested it on high volume production environments. This would need robust deduplication to avoid too many redundant tests being captured. We do have ideas on building a robust deduplication system [#27](https://github.com/keploy/keploy/issues/27)

## ✨ Resources!
🤔 [FAQs](https://keploy.io/docs/keploy-explained/faq/)

🕵️‍️ [Why Keploy](https://keploy.io/docs/keploy-explained/why-keploy/)

⚙️ [Installation Guide](https://keploy.io/docs/application-development/)

📖 [Contribution Guide](https://keploy.io/docs/keploy-explained/contribution-guide/)


## 🤔 Questions?
Reach out to us. We're here to help!

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-2poflru6f-_VAuvQfCBT8fDWv1WwSbkw)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![Twitter](https://img.shields.io/badge/Twitter-%231DA1F2.svg?style=for-the-badge&logo=Twitter&logoColor=white)](https://twitter.com/Keployio)


> ####  **If You Had Fun:** Please leave a 🌟 star on this repo! It's free and will bring a smile. 😄 👏
