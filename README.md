<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="keploy logo"/>  <!-- we can add banner here, maybe a poster or a gif -->
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

  
[![GitHub Stars](https://img.shields.io/github/stars/keploy/keploy?style=for-the-badge&logo=star&logoColor=yellow&color=000000&labelColor=000000&label=Help%20us%20reach%2010K%20stars!)](https://github.com/Keploy/Keploy/issues)
[![CNCF Landscape](https://img.shields.io/badge/CNCF_Landscape-0078D4?style=for-the-badge&logo=data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAyNCAyNCI+PHBhdGggZmlsbD0id2hpdGUiIGQ9Ik0xMiAyTDIgNy4wMDl2OS45ODJMMTIgMjJsMTAtNS4wMDlWNy4wMDlMMTIgMnptMCAxNi41TDQuNSAxNHYtNEwxMiA1LjVsNy41IDQuNXY0TDEyIDE4LjV6Ii8+PC9zdmc+)](https://landscape.cncf.io/?item=app-definition-and-development--continuous-integration-delivery--keploy)
[![Go Report Card](https://goreportcard.com/badge/go.keploy.io/server?style=for-the-badge)](https://goreportcard.com/report/go.keploy.io/server)
[![GitHub release (latest by date)](https://img.shields.io/github/v/release/keploy/keploy?style=for-the-badge)](https://github.com/keploy/keploy/releases)




[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-2poflru6f-_VAuvQfCBT8fDWv1WwSbkw)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![keployio on YouTube](https://img.shields.io/badge/keployio-FF0000?style=for-the-badge&logo=youtube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![keployio on X](https://img.shields.io/badge/keployio-black?style=for-the-badge&logo=x&logoColor=white)](https://x.com/keployio)

<!--[![keployio on YouTube](https://img.shields.io/badge/keployio-FF0000?style=flat&logo=youtube)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![Keploy on LinkedIn](https://img.shields.io/badge/Keploy-0A66C2?style=flat&logo=linkedin)](https://www.linkedin.com/company/keploy/) 
[![Keploy on Slack](https://img.shields.io/badge/Slack-4A154B?style=flat&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-2poflru6f-_VAuvQfCBT8fDWv1WwSbkw)
[![@keployio on X](https://img.shields.io/badge/%40keployio-black?style=for-the-badge&logo=x&logoColor=white)](https://x.com/keployio)-->


<!--[![Twitter](https://img.shields.io/badge/Twitter-%231DA1F2.svg?style=for-the-badge&logo=Twitter&logoColor=white)](https://twitter.com/Keployio)-->

<a href="https://trendshift.io/repositories/3262" target="_blank"><img src="https://trendshift.io/api/badge/repositories/3262" alt="keploy%2Fkeploy | Trendshift" style="width: 250px; height: 55px;" width="250" height="55"/></a>
</h4>


  <!-- we can add banner here, maybe a poster or a gif -->
  
[Keploy](https://keploy.io) is a **developer-centric** API testing tool designed to simplify and accelerate the testing process. By creating **tests with built-in mocks**, Keploy offers a faster alternative to traditional unit testing—and it keeps getting faster every day!  

Beyond recording API calls, Keploy captures database interactions and replays them during testing, ensuring seamless and reliable results. With the recent addition of a **VS Code AI-powered extension**, Keploy is now even more accessible, making it easier than ever to integrate into your workflow. It’s **easy to use, powerful, and highly extensible**, empowering developers to save time and focus on building great software.



> 🐰 **Fun fact:** Keploy uses itself for testing! Check out our swanky coverage badge: [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;


<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="Convert API calls to test cases"/>



<!--# 📜 Table of Contents  

- [🚀 Quick Installation](#-quick-installation)  
- [📘 Documentation](#-documentation)  
- [🌐 Language Support](#-language-support)  
- [🧡 Keploy Adopters](#-keploy-adopters-)
- [👨🏻‍💻 Contributing](#-lets-build-together-)
- [🐲 Current Limitations!](#limitations) 
- [📚 Resources](#-resources)  
- [❓ Questions](#-questions)  
---
-->

## 🚀 Quick Installation


Save time by Integrating Keploy seamlessly into your development workflow with no code changes required. Let's dive into its powerful features and how to use them one by one:


### 🧪 Unit Test Generation

Keploy introduces the world's first **unit test generator (ut-gen)**, based on the [Meta LLM research paper](https://arxiv.org/pdf/2402.09171). It understands code semantics to generate meaningful unit tests automatically, saving time and improving test quality.

#### Core features: 🛠

| **Feature**                  | **Description**                                                                 |
|------------------------------|---------------------------------------------------------------------------------|
| **Automate Unit Test Generation (UTG)** | Quickly generates comprehensive unit tests, reducing manual effort.               |
| **Improve Edge Cases**       | Covers complex scenarios with smarter test generation.                           |
| **Boost Test Coverage**      | Ensures exhaustive coverage for growing codebases.                               |

#### 🚀 How to Use the Unit Test Generator
1. **Install Keploy VS Code Extension:**  
   Get the [Keploy Unit Test Generator AI Extension](https://marketplace.visualstudio.com/items?itemName=Keploy.keployio) and add it to VS Code.

2. **Setup Keploy:**  
   Use Keploy locally or its cloud-hosted services for quick setup.

3. **Generate Tests:**  
   - Open a file in VS Code.  
   - Right-click and choose **"Generate Unit Test with Keploy"**.  
   - The extension will generate tests for your functions or code.

4. **Run and Validate:**  
   Execute the tests using your preferred test runner (e.g., Jest, Mocha) and refine edge cases if necessary.

Elevate your unit testing game with Keploy's **AI-powered VS Code Extension**!  

#### 📜 [Install the VS Code AI Extension for Unit Test Generation](https://marketplace.visualstudio.com/items?itemName=Keploy.keployio) and get started today! ✅ 
📜 Follow [Unit Test Generator README](README-UnitGen.md)! ✅ 

---

<!--### 🔗 Integration Testing  

Keploy simplifies integration testing by capturing and replaying **ALL** your app's network interactions, including CRUD operations and non-idempotent APIs. This ensures seamless communication between application components while detecting and addressing compatibility issues early.  

#### 🛠 How it Works:  
Keploy acts as a proxy that records your app's network interactions and replays them during testing to validate behavior. The magic lies in its ability to simulate real-world scenarios effortlessly!  

Take a journey to **[How Keploy Works?](https://keploy.io/docs/keploy-explained/how-keploy-works/)** to discover the tricks behind the curtain!

-->

### 🌐 Integration Testing  

Keploy automates API testing by recording API requests and responses during runtime. These recordings are transformed into reusable test cases, allowing you to validate your APIs efficiently.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="Record Replay Testing"/>

#### Core features: 🛠

| **Feature**                  | **Description**                                                                                                             |
|------------------------------|-----------------------------------------------------------------------------------------------------------------------------|
| ♻️ **Combined Test Coverage** | Merge your Keploy Tests with your favorite testing libraries (JUnit, go-test, py-test, jest) to see a combined test coverage. |
| 🤖 **EBPF Instrumentation**   | Keploy uses EBPF like a secret sauce to make integration code-less, language-agnostic, and lightweight.                      |
| 🌐 **CI/CD Integration**      | Run tests with mocks locally, in CI pipelines (e.g., Jenkins, GitHub Actions), or across Kubernetes clusters.                |
| 📽️ **Record-Replay Flows**   | Record and replay distributed API flows as mocks/stubs, like a time machine for your tests.                                  |
| 🎭 **Multi-Purpose Mocks**    | Use Keploy Mocks as server tests.  

#### 🎩 How's the Magic Happen?
Keploy proxy captures and replays **ALL** (CRUD operations, including non-idempotent APIs) of your app's network interactions.
You can also take a look at **[How Keploy Works?](https://keploy.io/docs/keploy-explained/how-keploy-works/)** to discover the tricks behind the curtain!
<!--#### **Steps to Get Started:**  
1. **Set Up Keploy Locally:** Install Keploy with one-click installation and minimal configuration.  
2. **Capture API Traffic:** Run your application while Keploy records all API requests and responses.  
3. **Generate Test Cases:** Use the captured data to create reusable test cases with mock data and stubs for validation.  
4. **Run Tests Anywhere:** Validate API behavior locally, in CI/CD pipelines, or even across Kubernetes clusters.-->  

Keploy ensures consistent API behavior, improves API quality, and saves manual testing effort. 🚀  

<!--Ready to dive deeper? Check out Keploy’s [API Testing Documentation](https://keploy.io/docs)!  -->

```shell
curl --silent -O -L https://keploy.io/install.sh && source install.sh
```

####  🎬 Recording Testcases

Start your app with Keploy to convert API calls as Tests and Mocks/Stubs.

```zsh
keploy record -c "CMD_TO_RUN_APP" 
```
For example, if you're using a simple Python app the `CMD_TO_RUN_APP` would resemble to `python main.py`, for  Golang `go run main.go`, for java `java -jar xyz.jar`, for node `npm start`..

```zsh
keploy record -c "python main.py"
```

#### 🧪 Running Tests
Shut down the databases, redis, kafka or any other services your application uses. Keploy doesn't need those during test.
```zsh
keploy test -c "CMD_TO_RUN_APP" --delay 10
```

#### ✅ Test Coverage Integration
To integrate with your unit-testing library and see combine test coverage, follow this [test-coverage guide](https://keploy.io/docs/server/sdk-installation/go/).

<img src="https://keploy.io/docs/img/oss/keploy-arch.png?raw=true" alt="Keploy Architecture" style="width: 100%;">


### One-Click Setup 🚀

Save time and effort! Run Keploy instantly without the need for any local installation. Get started in just a few clicks!

[![GitHub Codespace](https://img.shields.io/badge/GH%20codespace-3670A0?style=for-the-badge&logo=github&logoColor=fff)](https://github.dev/Sonichigo/mux-sql)

<!--<table border="0">
  <tr>
    <td align="center" width="100" height="100">
      <a href="https://github.dev/Sonichigo/mux-sql">
        <img
          width="50"
          height="50"
          src="https://devblogs.microsoft.com/cppblog/wp-content/uploads/sites/9/2022/04/github-vscode-mark.png"
          alt="GitHub Codespace Logo"
        />
        <br /><sub><b>GitHub Codespace</b></sub>
      </a>
    </td>
  </tr>
</table>-->


<!--## 🚨 Here for  [Unit Test Generator](README-UnitGen.md) (ut-gen)? -->


## 📘 Documentation!
Want to explore or learn more about Keploy? Become a Keploy pro with **[Keploy Documentation](https://keploy.io/docs/)**.




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

<!--
## 🎩 How's the Magic Happen?
Keploy proxy captures and replays **ALL** (CRUD operations, including non-idempotent APIs) of your app's network interactions.


Take a journey to **[How Keploy Works?](https://keploy.io/docs/keploy-explained/how-keploy-works/)** to discover the tricks behind the curtain!-->


## 👨🏻‍💻 Let's Build Together! 👩🏻‍💻
Whether you're a newbie coder or a wizard 🧙‍♀️, your perspective is golden. Take a peek at our:

📜 [Contribution Guidelines](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md)

❤️ [Code of Conduct](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)


## Limitations
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
