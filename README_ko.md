<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="keploy 로고"/>
</p>
<h3 align="center">
<b>
⚡️ 유닛 테스트보다 빠른 API 테스트, 사용자 트래픽에서 생성 ⚡️
</b>
</h3 >
<p align="center">
🌟 AI 시대 개발자를 위한 필수 도구 🌟
</p>

---

<h4 align="center">

<a href="https://x.com/Keployio">
    <img src="https://img.shields.io/badge/follow-%40keployio-1DA1F2?logo=X&style=social" alt="Keploy X!" />
  </a>

<a href="https://github.com/Keploy/Keploy/">
   <img src="https://img.shields.io/github/stars/keploy/keploy?color=%23EAC54F&logo=github&label=Help%20us%20reach%2020K%20stars!%20Now%20at:" alt="20K 스타 달성에 동참해주세요! 현재:" />
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

[Keploy](https://keploy.io)는 **개발자 중심**의 API 테스트 도구로, 유닛 테스트보다 빠르게 **내장된 목(mock)과 함께 테스트를 생성**합니다.

Keploy는 API 호출을 기록할 뿐만 아니라 데이터베이스 호출도 기록하고 테스트 중에 재생하여 **사용하기 쉽고 강력하며 확장 가능**하도록 합니다.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="Convert API calls to test cases"/>

> 🐰 **재미있는 사실:** Keploy는 자체 테스트에 Keploy를 사용합니다! 멋진 커버리지 배지를 확인해보세요: [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

## 🚨 [유닛 테스트 생성기](README-UnitGen.md)(ut-gen)를 찾으시나요?

Keploy는 최근 세계 최초로 [Meta LLM 연구 논문](https://arxiv.org/pdf/2402.09171)의 유닛 테스트 생성기(ut-gen) 구현을 출시했습니다. 이는 코드 의미를 이해하고 의미 있는 유닛 테스트를 생성하며, 다음과 같은 목표를 가지고 있습니다:

- **유닛 테스트 생성(UTG) 자동화**: 포괄적인 유닛 테스트를 빠르게 생성하고 수동 작업의 중복을 줄입니다.

- **에지 케이스 개선**: 자동화된 테스트 범위를 확장 및 개선하여 수동으로는 놓치기 쉬운 복잡한 시나리오를 다룹니다.

- **테스트 커버리지 향상**: 코드베이스가 성장함에 따라 철저한 커버리지 보장이 가능해져 우리의 미션과 일치합니다.

### 📜 [유닛 테스트 생성기 README](README-UnitGen.md)를 따라가세요! ✅

## 📘 문서!

**[Keploy 문서](https://keploy.io/docs/)**를 통해 Keploy 전문가가 되어보세요.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="Record Replay Testing"/>

# 🚀 빠른 설치 (API 테스트 생성기)

로컬에 에이전트를 설치하여 Keploy를 통합하세요. 코드 변경이 필요하지 않습니다.

```shell
curl --silent -O -L https://keploy.io/install.sh && source install.sh
```

##  🎬 테스트케이스 기록

Keploy로 앱을 시작하여 API 호출을 테스트 및 모의(Mock)/스텁(Stub)으로 변환하세요.

```zsh
keploy record -c "CMD_TO_RUN_APP" 
```

예를 들어, 간단한 Python 앱을 사용하는 경우 `CMD_TO_RUN_APP`은 `python main.py`와 유사할 것이며, Golang의 경우 `go run main.go`, Java의 경우 `java -jar xyz.jar`, Node의 경우 `npm start`와 같습니다.

```zsh
keploy record -c "python main.py"
```

## � 테스트 실행

애플리케이션이 사용하는 데이터베이스, Redis, Kafka 또는 기타 서비스를 종료하세요. Keploy는 테스트 중에 이러한 서비스가 필요하지 않습니다.

```zsh
keploy test -c "CMD_TO_RUN_APP" --delay 10
```

## ✅ 테스트 커버리지 통합

단위 테스트 라이브러리와 통합하고 결합된 테스트 커버리지를 확인하려면 [테스트 커버리지 가이드](https://keploy.io/docs/server/sdk-installation/go/)를 따르세요.

> #### **즐거웠다면:** 이 저장소에 🌟 별을 남겨주세요! 무료이며 미소를 가져다 줄 거예요. 😄 👏

## 원클릭 설정 🚀

로컬 머신 설치 없이 Keploy를 빠르게 설정하고 실행하세요:

[![GitHub Codescape](https://img.shields.io/badge/GH%20codespace-3670A0?style=for-the-badge&logo=github&logoColor=fff)]([https://github.dev/Sonichigo/mux-sql](https://github.dev/Sonichigo/mux-sql))

## 🤔 질문이 있으신가요?

저희에게 연락하세요. 도와드리겠습니다!

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)

## 🌐 언어 지원

Go의 고퍼 🐹부터 Python의 뱀 🐍까지 지원합니다:

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)
![Rust](https://img.shields.io/badge/Rust-darkred?style=for-the-badge&logo=rust&logoColor=white)
![C#](https://img.shields.io/badge/csharp-purple?style=for-the-badge&logo=csharp&logoColor=white)
![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54)

## 🫰 Keploy 사용자 🧡

당신과 당신의 조직이 Keploy를 사용하고 있다고요? 정말 멋집니다. [**이 목록**](https://github.com/orgs/keploy/discussions/1765)에 추가해 주시면, 선물을 보내드리겠습니다! 💖

여러분이 우리 커뮤니티의 일원이 되어 주셔서 기쁘고 자랑스럽습니다! 💖

## 🎩 마법은 어떻게 일어날까요?

Keploy 프록시는 앱의 네트워크 상호작용 **전체**(CRUD 작업, 비멱등성 API 포함)를 캡처하고 재생합니다.

**[Keploy 작동 방식](https://keploy.io/docs/keploy-explained/how-keploy-works/)**으로 여행을 떠나, 무대 뒤의 비밀을 발견해 보세요!

## 🔧 핵심 기능

- ♻️ **통합 테스트 커버리지:** Keploy 테스트를 선호하는 테스트 라이브러리(JUnit, go-test, py-test, jest)와 결합하여 통합 테스트 커버리지를 확인하세요.  

- 🤖 **EBPF 계측:** Keploy는 EBPF를 비밀 소스처럼 사용해 통합을 코드 없이, 언어 중립적이며 매우 가볍게 만듭니다.  

- 🌐 **CI/CD 통합:** CLI에서 로컬로, CI 파이프라인(Jenkins, Github Actions 등)에서, 심지어 Kubernetes 클러스터 전반에서 모의 테스트를 실행하세요.  

- 📽️ **복잡한 흐름 기록 및 재생:** Keploy는 분산된 복잡한 API 흐름을 모의 및 스텁으로 기록하고 재생할 수 있습니다. 테스트를 위한 타임머신을 갖춘 것처럼 시간을 엄청나게 절약해 줍니다!  

- 🎭 **다목적 모의:** Keploy에서 생성된 모의를 서버 테스트로도 사용할 수 있습니다!

👉 **GitHub에서 코드 탐색하기**: [github.com/keploy/keploy](https://github.com/keploy/keploy)

## 👨🏻‍💻 함께 만들어봐요! 👩🏻‍💻

초보 개발자이든 전문가 🧙‍♀️이든, 여러분의 관점은 소중합니다. 아래 내용을 확인해보세요:

📜 [기여 가이드라인](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md)

❤️ [행동 강령](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)

## 🐲 현재 한계점!

- **유닛 테스팅:** Keploy는 유닛 테스트 프레임워크(Go test, JUnit 등)와 함께 실행되도록 설계되었으며 전체 코드 커버리지를 높일 수 있지만, 여전히 통합 테스트를 생성합니다.
- **프로덕션 환경:** Keploy는 현재 개발자를 위한 테스트 생성에 중점을 두고 있습니다. 어떤 환경에서도 테스트를 캡처할 수 있지만, 대량의 프로덕션 환경에서는 테스트되지 않았습니다. 너무 많은 중복 테스트가 캡처되는 것을 방지하려면 강력한 중복 제거 시스템이 필요합니다. 우리는 강력한 중복 제거 시스템 구축에 대한 아이디어를 가지고 있습니다 [#27](https://github.com/keploy/keploy/issues/27)

## ✨ 리소스!

🤔 [FAQ](https://keploy.io/docs/keploy-explained/faq/)

🕵️‍️ [Keploy를 선택하는 이유](https://keploy.io/docs/keploy-explained/why-keploy/)

⚙️ [설치 가이드](https://keploy.io/docs/application-development/)

📖 [기여 가이드](https://keploy.io/docs/keploy-explained/contribution-guide/)