<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="logo keploy"/>
</p>
<h3 align="center">
<b>
⚡️ Testes de API mais rápidos que testes unitários, a partir do tráfego do usuário ⚡️
</b>
</h3 >
<p align="center">
🌟 A ferramenta essencial para desenvolvedores na era da IA-Gen 🌟
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

[Keploy](https://keploy.io) é uma ferramenta de teste de API **centrada no desenvolvedor** que cria **testes junto com mocks incorporados**, mais rápido do que testes unitários.

O Keploy não apenas grava chamadas de API, mas também registra chamadas de banco de dados e as reproduz durante os testes, tornando-o **fácil de usar, poderoso e extensível**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="Convert API calls to test cases"/>

> 🐰 **Curiosidade:** O Keploy se utiliza para testes! Confira nosso incrível emblema de cobertura: [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

## 🚨 Veio para o [Gerador de Testes Unitários](README-UnitGen.md) (ut-gen)?

O Keploy acaba de lançar a primeira implementação mundial de um gerador de testes unitários (ut-gen) baseado no [artigo de pesquisa Meta LLM](https://arxiv.org/pdf/2402.09171), que compreende a semântica do código e gera testes unitários significativos, com o objetivo de:

- **Automatizar a geração de testes unitários (UTG)**: Gerar rapidamente testes unitários abrangentes e reduzir o esforço manual redundante.

- **Melhorar casos extremos**: Ampliar e melhorar o escopo dos testes automatizados para cobrir cenários mais complexos, frequentemente negligenciados manualmente.

- **Aumentar a cobertura de testes**: À medida que as bases de código crescem, garantir uma cobertura exaustiva deve se tornar viável, alinhando-se à nossa missão.

### 📜 Siga o [README do Gerador de Testes Unitários](README-UnitGen.md)! ✅

## 📘 Documentação!

Torne-se um especialista em Keploy com a **[Documentação do Keploy](https://keploy.io/docs/)**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="Record Replay Testing"/>

# 🚀 Instalação Rápida (gerador de testes de API)

Integre o Keploy instalando o agente localmente. Nenhuma alteração de código necessária.

```shell
curl --silent -O -L https://keploy.io/install.sh && source install.sh
```

##  🎬 Gravando Casos de Teste

Inicie seu aplicativo com o Keploy para converter chamadas de API em Testes e Mocks/Stubs.

```zsh
keploy record -c "CMD_TO_RUN_APP" 
```

Por exemplo, se você estiver usando um aplicativo Python simples, o `CMD_TO_RUN_APP` seria semelhante a `python main.py`, para Golang `go run main.go`, para Java `java -jar xyz.jar`, para Node `npm start`..

```zsh
keploy record -c "python main.py"
```

## 🧪 Executando Testes

Desligue os bancos de dados, Redis, Kafka ou quaisquer outros serviços que seu aplicativo utiliza. O Keploy não precisa deles durante os testes.

```zsh
keploy test -c "CMD_TO_RUN_APP" --delay 10
```

## ✅ Integração de Cobertura de Testes

Para integrar com sua biblioteca de testes unitários e ver a cobertura de testes combinada, siga este [guia de cobertura de testes](https://keploy.io/docs/server/sdk-installation/go/).

> ####  **Se Você se Divertiu:** Por favor, deixe uma 🌟 estrela neste repositório! É gratuito e trará um sorriso. 😄 👏

## Configuração com Um Clique 🚀

Configure e execute o keploy rapidamente, sem necessidade de instalação na máquina local:

[![GitHub Codescape](https://img.shields.io/badge/GH%20codespace-3670A0?style=for-the-badge&logo=github&logoColor=fff)]([https://github.dev/Sonichigo/mux-sql](https://github.dev/Sonichigo/mux-sql))

## 🤔 Dúvidas?

Entre em contato conosco. Estamos aqui para ajudar!

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)

## 🌐 Suporte a Idiomas

Do gopher do Go 🐹 à cobra do Python 🐍, nós suportamos:

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)
![Rust](https://img.shields.io/badge/Rust-darkred?style=for-the-badge&logo=rust&logoColor=white)
![C#](https://img.shields.io/badge/csharp-purple?style=for-the-badge&logo=csharp&logoColor=white)
![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54)

## 🫰 Adotantes do Keploy 🧡

Então você e sua organização estão usando o Keploy? Isso é ótimo. Por favor, adicione-se a [**esta lista,**](https://github.com/orgs/keploy/discussions/1765) e enviaremos brindes para vocês! 💖

Estamos felizes e orgulhosos por ter todos vocês como parte da nossa comunidade! 💖

## 🎩 Como a Mágica Acontece?

O proxy do Keploy captura e reproduz **TODAS** (operações CRUD, incluindo APIs não idempotentes) as interações de rede do seu aplicativo.

Faça uma jornada para **[Como o Keploy Funciona?](https://keploy.io/docs/keploy-explained/how-keploy-works/)** para descobrir os truques por trás das cortinas!

## 🔧 Funcionalidades Principais

- ♻️ **Cobertura de Testes Combinada:** Combine seus Testes do Keploy com suas bibliotecas de teste favoritas (JUnit, go-test, py-test, jest) para ver uma cobertura de testes combinada.

- 🤖 **Instrumentação EBPF:** O Keploy usa EBPF como um ingrediente secreto para tornar a integração sem código, independente de linguagem e super leve.

- 🌐 **Integração CI/CD:** Execute testes com mocks onde quiser—localmente no CLI, no seu pipeline de CI (Jenkins, Github Actions..), ou até mesmo em um cluster Kubernetes.

- 📽️ **Gravar-Reproduzir Fluxos Complexos:** O Keploy pode gravar e reproduzir fluxos de API complexos e distribuídos como mocks e stubs. É como ter uma máquina do tempo para seus testes—economizando muito tempo!

- 🎭 **Mocks Multiuso:** Você também pode usar os Mocks gerados pelo Keploy como Testes de servidor!

👉 **Explore o código no GitHub**: [github.com/keploy/keploy](https://github.com/keploy/keploy)

## 👨🏻‍💻 Vamos Construir Juntos! 👩🏻‍💻

Seja você um iniciante em programação ou um mago 🧙‍♀️, sua perspectiva é valiosa. Dê uma olhada em nossos:

📜 [Diretrizes de Contribuição](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md)

❤️ [Código de Conduta](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)

## 🐲 Limitações Atuais!

- **Testes Unitários:** Embora o Keploy seja projetado para funcionar junto com frameworks de teste unitário (Go test, JUnit...) e possa aumentar a cobertura geral de código, ele ainda gera testes de integração.
- **Ambientes de Produção:** Atualmente, o Keploy está focado em gerar testes para desenvolvedores. Esses testes podem ser capturados em qualquer ambiente, mas ainda não o testamos em ambientes de produção com alto volume. Isso exigiria um sistema robusto de deduplicação para evitar a captura de muitos testes redundantes. Temos ideias para construir um sistema robusto de deduplicação [#27](https://github.com/keploy/keploy/issues/27)

## ✨ Recursos!

🤔 [Perguntas Frequentes](https://keploy.io/docs/keploy-explained/faq/)

🕵️‍️ [Por que o Keploy](https://keploy.io/docs/keploy-explained/why-keploy/)

⚙️ [Guia de Instalação](https://keploy.io/docs/application-development/)

📖 [Guia de Contribuição](https://keploy.io/docs/keploy-explained/contribution-guide/)