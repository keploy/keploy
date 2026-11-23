<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="Keploy-Logo"/>
</p>
<h3 align="center">
<b>
⚡️ API-Tests schneller als Unit-Tests, aus Nutzerverkehr generiert ⚡️
</b>
</h3 >
<p align="center">
🌟 Das unverzichtbare Tool für Entwickler im KI-Zeitalter 🌟
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

[Keploy](https://keploy.io) ist ein **entwicklerzentriertes** API-Testtool, das **Tests mit integrierten Mocks** erstellt, schneller als Unit-Tests.

Keploy zeichnet nicht nur API-Aufrufe auf, sondern auch Datenbankaufrufe und spielt sie während des Testens wieder ab, was es **einfach zu bedienen, leistungsstark und erweiterbar** macht.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="API-Aufrufe in Testfälle umwandeln"/>

> 🐰 **Fun Fact:** Keploy nutzt sich selbst für Tests! Schaut euch unser cooles Coverage-Badge an: [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

## 🚨 Hier für den [Unit Test Generator](README-UnitGen.md) (ut-gen)?

Keploy hat kürzlich die weltweit erste Implementierung eines Unit-Test-Generators (ut-gen) basierend auf dem [Meta LLM-Forschungspapier](https://arxiv.org/pdf/2402.09171) veröffentlicht. Er versteht Code-Semantik und generiert sinnvolle Unit-Tests mit dem Ziel:

- **Automatisierung der Unit-Test-Generierung (UTG):** Schnelle Generierung umfassender Unit-Tests und Reduzierung redundanter manueller Arbeit.

- **Verbesserung von Edge Cases:** Erweiterung und Verbesserung des Umfangs automatisierter Tests, um komplexere Szenarien abzudecken, die manuell oft übersehen werden.

- **Steigerung der Testabdeckung:** Mit wachsenden Codebasen sollte eine erschöpfende Abdeckung machbar werden, entsprechend unserer Mission.

### 📜 Folge dem [Unit Test Generator README](README-UnitGen.md)! ✅

## 📘 Dokumentation!

Werde ein Keploy-Profi mit der **[Keploy-Dokumentation](https://keploy.io/docs/)**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="Record Replay Testing"/>

# 🚀 Schnelle Installation (API-Testgenerator)

Integriere Keploy durch lokale Installation des Agents. Keine Code-Änderungen erforderlich.

```shell
curl --silent -O -L https://keploy.io/install.sh && source install.sh
```

## 🎬 Aufzeichnung von Testfällen

Starten Sie Ihre App mit Keploy, um API-Aufrufe in Tests und Mocks/Stubs umzuwandeln.

```zsh
keploy record -c "CMD_TO_RUN_APP" 
```

Beispielsweise, wenn Sie eine einfache Python-App verwenden, würde der `CMD_TO_RUN_APP` etwa `python main.py` entsprechen, für Golang `go run main.go`, für Java `java -jar xyz.jar`, für Node `npm start`.

```zsh
keploy record -c "python main.py"
```

## 🧪 Tests ausführen

Fahren Sie die Datenbanken, Redis, Kafka oder andere Dienste, die Ihre Anwendung nutzt, herunter. Keploy benötigt diese während des Tests nicht.

```zsh
keploy test -c "CMD_TO_RUN_APP" --delay 10
```

## ✅ Integration der Testabdeckung

Um die Integration mit Ihrer Unit-Testing-Bibliothek durchzuführen und die kombinierte Testabdeckung zu sehen, folgen Sie dieser [Anleitung zur Testabdeckung](https://keploy.io/docs/server/sdk-installation/go/).

> ####  **Wenn es Ihnen Spaß gemacht hat:** Hinterlassen Sie bitte einen 🌟 Stern auf diesem Repo! Es ist kostenlos und bringt ein Lächeln. 😄 👏

## Ein-Klick-Setup 🚀

Richten Sie Keploy schnell ein und führen Sie es aus, ohne Installation auf dem lokalen Rechner:

[![GitHub Codescape](https://img.shields.io/badge/GH%20codespace-3670A0?style=for-the-badge&logo=github&logoColor=fff)]([https://github.dev/Sonichigo/mux-sql](https://github.dev/Sonichigo/mux-sql))

## 🤔 Fragen?

Kontaktieren Sie uns. Wir helfen Ihnen gerne!

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)

## 🌐 Sprachunterstützung

Von Go's Gopher 🐹 bis zu Pythons Schlange 🐍, wir unterstützen:

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)
![Rust](https://img.shields.io/badge/Rust-darkred?style=for-the-badge&logo=rust&logoColor=white)
![C#](https://img.shields.io/badge/csharp-purple?style=for-the-badge&logo=csharp&logoColor=white)
![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54)

## 🫰 Keploy-Anwender 🧡

Nutzt du oder deine Organisation Keploy? Das ist großartig. Tragt euch bitte in [**diese Liste**](https://github.com/orgs/keploy/discussions/1765) ein, und wir senden euch Goodies! 💖

Wir freuen uns und sind stolz darauf, euch alle als Teil unserer Community zu haben! 💖

## 🎩 Wie funktioniert die Magie?

Der Keploy-Proxy erfasst und spielt **ALLE** (CRUD-Operationen, einschließlich nicht-idempotenter APIs) Netzwerkinteraktionen deiner App nach.

Begib dich auf die Reise zu **[Wie Keploy funktioniert?](https://keploy.io/docs/keploy-explained/how-keploy-works/)**, um die Tricks hinter den Kulissen zu entdecken!

## 🔧 Kernfunktionen

- ♻️ **Kombinierte Testabdeckung:** Verbinde deine Keploy-Tests mit deinen Lieblingstestbibliotheken (JUnit, go-test, py-test, jest), um eine kombinierte Testabdeckung zu sehen.  

- 🤖 **EBPF-Instrumentierung:** Keploy nutzt EBPF wie eine geheime Zutat, um Integration ohne Code, sprachunabhängig und superleichtgewichtig zu machen.  

- 🌐 **CI/CD-Integration:** Führe Tests mit Mocks aus, wo immer du möchtest – lokal auf der CLI, in deiner CI-Pipeline (Jenkins, Github Actions...) oder sogar über einen Kubernetes-Cluster hinweg.  

- 📽️ **Aufzeichnen-Wiedergeben komplexer Abläufe:** Keploy kann komplexe, verteilte API-Abläufe als Mocks und Stubs aufzeichnen und wiedergeben. Es ist, als hättest du eine Zeitmaschine für deine Tests – und sparst dabei jede Menge Zeit!  

- 🎭 **Vielseitige Mocks:** Du kannst die von Keploy generierten Mocks auch als Server-Tests verwenden!

👉 **Entdecke den Code auf GitHub**: [github.com/keploy/keploy](https://github.com/keploy/keploy)

## 👨🏻‍💻 Lass uns gemeinsam bauen! 👩🏻‍💻

Egal, ob du ein Coding-Neuling oder ein Zauberer 🧙‍♀️ bist – deine Perspektive ist Gold wert. Wirf einen Blick auf unsere:

📜 [Beitragsrichtlinien](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md)

❤️ [Verhaltenskodex](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)

## 🐲 Aktuelle Einschränkungen!

- **Unit-Tests:** Obwohl Keploy dafür ausgelegt ist, neben Unit-Test-Frameworks (Go test, JUnit usw.) zu laufen und die Gesamttestabdeckung zu erhöhen, generiert es weiterhin Integrationstests.
- **Produktionsumgebungen:** Keploy konzentriert sich derzeit auf die Generierung von Tests für Entwickler. Diese Tests können aus jeder Umgebung erfasst werden, aber wir haben sie noch nicht in hochvolumigen Produktionsumgebungen getestet. Hier wäre eine robuste Deduplizierung erforderlich, um zu viele redundante Tests zu vermeiden. Wir haben Ideen für ein robustes Deduplizierungssystem [#27](https://github.com/keploy/keploy/issues/27)

## ✨ Ressourcen!

🤔 [FAQs](https://keploy.io/docs/keploy-explained/faq/)

🕵️‍️ [Warum Keploy](https://keploy.io/docs/keploy-explained/why-keploy/)

⚙️ [Installationsanleitung](https://keploy.io/docs/application-development/)

📖 [Beitragsleitfaden](https://keploy.io/docs/keploy-explained/contribution-guide/)