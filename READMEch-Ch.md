<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="keploy logo"/>
</p>
<h3 align="center">
<b>
⚡️ 通过用户流量生成 API 测试，速度远超单元测试 ⚡️
</b>
</h3>
<p align="center">
🌟 AI 原生时代开发者的必备工具 🌟
</p>

---

<h4 align="center">

   <a href="https://x.com/Keployio">
    <img src="https://img.shields.io/badge/follow-%40keployio-1DA1F2?logo=X&style=social" alt="Keploy X" />
  </a>

<a href="https://github.com/Keploy/Keploy/">
   <img src="https://img.shields.io/github/stars/keploy/keploy?color=%23EAC54F&logo=github&label=帮助我们达到 20K Stars! 当前进度:" alt="Help us reach 20k stars!" />
  </a>

  <a href="https://landscape.cncf.io/?item=app-definition-and-development--continuous-integration-delivery--keploy">
    <img src="https://img.shields.io/badge/CNCF%20Landscape-5699C6?logo=cncf&style=social" alt="Keploy CNCF Landscape" />
  </a>

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)

</h4>

[Keploy](https://keploy.io) 是一款**以开发者为中心**的 API 测试工具，它通过**内置存根（Mocks）**生成测试用例，速度比编写单元测试快得多。

Keploy 不仅记录 API 调用，还能记录数据库查询并在测试期间回放，这使得它**易于使用、功能强大且具有良好的扩展性**。

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="将 API 调用转换为测试用例"/>

> 🐰 **有趣的事实：** Keploy 也在使用自己进行测试！看看我们出色的覆盖率徽章： [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

## 🚨 你是为了 [单元测试生成器](README-UnitGen.md) (ut-gen) 而来的吗？
Keploy 最近发布了全球首个基于 [Meta LLM 研究论文](https://arxiv.org/pdf/2402.09171) 的单元测试生成器 (ut-gen) 实现。它可以理解代码语义并生成有意义的单元测试。我们的目标是：

- **自动化单元测试生成 (UTG)**：快速生成全面的单元测试，减少冗余的手动工作。
- **改善边界情况**：扩展自动化测试范围，覆盖手动测试容易忽略的复杂场景。
- **提高测试覆盖率**：随着代码库的增长，确保能够进行彻底的覆盖。

### 📜 请参考 [单元测试生成器 README](README-UnitGen.md)！ ✅

## 📘 文档
访问 **[Keploy 文档](https://keploy.io/docs/)** 成为 Keploy 专家。

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="录制回放测试"/>

# 🚀 快速安装 (API 测试生成器)

在本地安装 Agent 以集成 Keploy，无需修改代码。

```shell
curl --silent -O -L [https://keploy.io/install.sh](https://keploy.io/install.sh) && source install.sh