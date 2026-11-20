<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="keploy logo"/>
</p>
<h3 align="center">
<b>
⚡️ Testes de API a partir do tráfego de usuários, mais rápido que testes unitários ⚡️
</b>
</h3>
<p align="center">
🌟 Ferramenta essencial para desenvolvedores na era AI-Native 🌟
</p>

---

<h4 align="center">

   <a href="https://x.com/Keployio">
    <img src="https://img.shields.io/badge/follow-%40keployio-1DA1F2?logo=X&style=social" alt="Keploy X" />
  </a>

<a href="https://github.com/Keploy/Keploy/">
   <img src="https://img.shields.io/github/stars/keploy/keploy?color=%23EAC54F&logo=github&label=Ajude-nos%20a%20chegar%20a%2020K%20stars!%20Status%20atual:" alt="Ajude-nos a chegar a 20k stars!" />
  </a>

  <a href="https://landscape.cncf.io/?item=app-definition-and-development--continuous-integration-delivery--keploy">
    <img src="https://img.shields.io/badge/CNCF%20Landscape-5699C6?logo=cncf&style=social" alt="Keploy CNCF Landscape" />
  </a>

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)

</h4>

[Keploy](https://keploy.io) é uma ferramenta de teste de API **focada no desenvolvedor** que cria casos de teste com **mocks integrados** de forma muito mais rápida do que escrever testes unitários.

O Keploy não apenas registra chamadas de API, mas também captura consultas a bancos de dados e as reproduz durante os testes, tornando-o **fácil de usar, poderoso e escalável**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="Converter chamadas de API em casos de teste"/>

> 🐰 **Fato Curioso:** O Keploy usa a si mesmo para testes! Confira nosso incrível selo de cobertura: [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

## 🚨 Você está aqui pelo [Gerador de Testes Unitários](README-UnitGen.md) (ut-gen)?
O Keploy lançou recentemente a primeira implementação mundial de um gerador de testes unitários (ut-gen) baseado no [artigo de pesquisa Meta LLM](https://arxiv.org/pdf/2402.09171). Ele entende a semântica do código e gera testes unitários significativos. Nossos objetivos são:

- **Automação da Geração de Testes Unitários (UTG)**: Gere testes unitários abrangentes rapidamente, reduzindo o esforço manual redundante.
- **Melhoria de Casos de Borda**: Expanda o alcance dos testes automatizados para cobrir cenários complexos frequentemente ignorados manualmente.
- **Aumento da Cobertura de Testes**: Garanta uma cobertura completa à medida que sua base de código cresce.

### 📜 Siga o [README do Gerador de Testes Unitários](README-UnitGen.md)! ✅

## 📘 Documentação!
Torne-se um mestre no Keploy com a **[Documentação do Keploy](https://keploy.io/docs/)**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="Teste de Gravação e Reprodução"/>

# 🚀 Instalação Rápida (Gerador de Testes de API)

Instale o agente localmente para integrar o Keploy. Nenhuma alteração de código é necessária.

```shell
curl --silent -O -L [https://keploy.io/install.sh](https://keploy.io/install.sh) && source install.sh