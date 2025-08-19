<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="keploy logo"/>
</p>
<h3 align="center">
<b>
⚡️ 比单元测试更快的API测试工具，源自真实用户流量 ⚡️
</b>
</h3 >
<p align="center">
🌟 AI时代开发者必备神器 🌟
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

[Keploy](https://keploy.io) 是一款**以开发者为中心**的 API 测试工具，能**快速生成包含内置模拟数据的测试用例**，速度远超单元测试。

Keploy 不仅能记录 API 调用，还能记录数据库操作并在测试时回放，使其**简单易用、功能强大且高度可扩展**。

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="将 API 调用转换为测试用例"/>

> 🐰 **有趣的事实：** Keploy 使用自身进行测试！看看我们炫酷的覆盖率徽章：[![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

## 🚨 你是来找 [单元测试生成器](README-UnitGen.md) (ut-gen) 的吗？

Keploy 最新推出了全球首个基于 [Meta LLM 研究论文](https://arxiv.org/pdf/2402.09171) 实现的单元测试生成器(ut-gen)，它能理解代码语义并生成有意义的单元测试，旨在：

- **自动化单元测试生成 (UTG)**：快速生成全面的单元测试，减少冗余的手动工作。

- **提升边缘案例覆盖**：扩展和改进自动化测试范围，覆盖更多复杂场景（这些场景往往容易被手动测试遗漏）。

- **提高测试覆盖率**：随着代码库增长，确保全面覆盖应变得可行，这与我们的使命一致。

### 📜 请移步 [单元测试生成器 README](README-UnitGen.md)！ ✅

## 📘 文档指南！

通过 **[Keploy 文档](https://keploy.io/docs/)** 成为 Keploy 专家。

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="记录回放测试"/>

# 🚀 快速安装 (API 测试生成器)

通过本地安装代理来集成 Keploy。无需代码变更。

```shell
curl --silent -O -L https://keploy.io/install.sh && source install.sh
```

##  🎬 记录测试用例

使用 Keploy 启动您的应用，将 API 调用转换为测试用例和模拟/桩数据。

```zsh
keploy record -c "CMD_TO_RUN_APP" 
```

例如，如果您使用的是简单的 Python 应用，`CMD_TO_RUN_APP` 类似于 `python main.py`；对于 Golang 是 `go run main.go`，Java 是 `java -jar xyz.jar`，Node 则是 `npm start`。

```zsh
keploy record -c "python main.py"
```

## 🧪 运行测试

请关闭您的应用所使用的数据库、Redis、Kafka 或其他服务。Keploy 在测试期间不需要这些依赖。

```zsh
keploy test -c "CMD_TO_RUN_APP" --delay 10
```

## ✅ 测试覆盖率集成

要与您的单元测试库集成并查看合并的测试覆盖率，请遵循此 [测试覆盖率指南](https://keploy.io/docs/server/sdk-installation/go/)。

> ####  **如果您觉得有趣：** 请给本仓库点个 🌟 star！完全免费，还能带来笑容。😄 👏

## 一键设置 🚀

快速设置并运行 keploy，无需在本地机器安装：

[![GitHub Codescape](https://img.shields.io/badge/GH%20codespace-3670A0?style=for-the-badge&logo=github&logoColor=fff)]([https://github.dev/Sonichigo/mux-sql](https://github.dev/Sonichigo/mux-sql))

## 🤔 有问题？

随时联系我们，我们随时为您提供帮助！

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)

## 🌐 语言支持

从 Go 的土拨鼠 🐹 到 Python 的蛇 🐍，我们支持：

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)
![Rust](https://img.shields.io/badge/Rust-darkred?style=for-the-badge&logo=rust&logoColor=white)
![C#](https://img.shields.io/badge/csharp-purple?style=for-the-badge&logo=csharp&logoColor=white)
![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54)

## 🫰 Keploy 采用者 🧡

你和你的团队正在使用 Keploy？太棒了。请将你们的信息添加到[**这个列表**](https://github.com/orgs/keploy/discussions/1765)，我们会送上精美礼品！💖

我们非常高兴和自豪能拥有你们这样的社区成员！💖

## 🎩 魔法如何实现？

Keploy 代理会捕获并重放你应用程序**所有**的网络交互（包括 CRUD 操作和非幂等 API）。

前往 **[Keploy 工作原理](https://keploy.io/docs/keploy-explained/how-keploy-works/)** 一探幕后玄机！

## 🔧 核心功能

- ♻️ **合并测试覆盖率**：将 Keploy 测试与你喜爱的测试库（JUnit、go-test、py-test、jest）结合，查看综合测试覆盖率。

- 🤖 **EBPF 插桩**：Keploy 使用 EBPF 作为秘密武器，实现无代码、语言无关且极其轻量级的集成。

- 🌐 **CI/CD 集成**：在任意环境运行测试——本地 CLI、CI 流水线（Jenkins、Github Actions...）甚至跨 Kubernetes 集群。

- 📽️ **记录-重放复杂流程**：Keploy 能记录并重放复杂的分布式 API 流程作为模拟和桩。就像为测试配备了时光机——节省大量时间！

- 🎭 **多功能模拟**：你还可以将 Keploy 生成的模拟用作服务器测试！

👉 **GitHub 代码仓库**：[github.com/keploy/keploy](https://github.com/keploy/keploy)

## 👨🏻‍💻 一起构建吧！👩🏻‍💻

无论你是编程新手还是技术大牛🧙‍♀️，你的视角都弥足珍贵。欢迎查阅：

📜 [贡献指南](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md)

❤️ [行为准则](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)

## 🐲 当前限制！

- **单元测试**：虽然Keploy设计为可与单元测试框架（Go test、JUnit等）并行运行，并能提升整体代码覆盖率，但它生成的仍是集成测试。
- **生产环境**：Keploy当前主要面向开发者生成测试。这些测试可从任何环境捕获，但我们尚未在高流量生产环境中进行验证。这需要强大的去重机制来避免捕获过多冗余测试。我们已规划构建健壮的去重系统 [#27](https://github.com/keploy/keploy/issues/27)

## ✨ 资源导航！

🤔 [常见问题](https://keploy.io/docs/keploy-explained/faq/)

🕵️‍️ [为什么选择Keploy](https://keploy.io/docs/keploy-explained/why-keploy/)

⚙️ [安装指南](https://keploy.io/docs/application-development/)

📖 [贡献指南](https://keploy.io/docs/keploy-explained/contribution-guide/)