
<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="keploy logo"/>
</p>
<h3 align="center">
<b>
⚡️ Tests d'API plus rapides que les tests unitaires, basés sur le trafic utilisateur ⚡️
</b>
</h3 >
<p align="center">
🌟 L'outil incontournable pour les développeurs à l'ère de l'IA 🌟
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


[Keploy](https://keploy.io) est un outil de test d'API **centré sur les développeurs** qui crée des **tests accompagnés de mocks intégrés**, plus rapidement que les tests unitaires.

Keploy n'enregistre pas seulement les appels d'API, mais également les appels de base de données et les rejoue lors des tests, ce qui le rend **facile à utiliser, puissant et extensible**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="Convert API calls to test cases"/>

> 🐰 **Fun fact:** Keploy se teste lui-même ! Admirez notre superbe badge de couverture : [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

## 🚨 Ici pour le [générateur de tests unitaires](README-UnitGen.md) (ut-gen) ? 
Keploy a récemment lancé le tout premier générateur de tests unitaires (ut-gen) au monde, implémentant le [Meta LLM research paper](https://arxiv.org/pdf/2402.09171), il comprend la sémantique du code et génère des tests unitaires pertinents, visant à :

- **Automatiser la génération de tests unitaires (UTG)** : Générer rapidement des tests unitaires complets et réduire les tests manuels redondants.

- **Améliorer les cas limites** : Étendre la portée des tests automatisés pour couvrir des scénarios complexes souvent oubliés.

- **Augmenter la couverture de tests** : Assurer une couverture exhaustive à mesure que les bases de code grandissent.

### 📜 Consultez le [README du générateur de tests unitaires](README-UnitGen.md) ! ✅

## 📘 Documentation !
Maîtrisez Keploy grâce à la **[Documentation Keploy](https://keploy.io/docs/)**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="Record Replay Testing"/>

# 🚀 Installation rapide (API test generator)

Intégrez Keploy en installant l'agent localement. Aucun changement de code n'est requis.

```shell
curl --silent -O -L https://keploy.io/install.sh && source install.sh
```

##  🎬 Enregistrer les cas de test

Lancez votre application avec Keploy pour transformer les appels d'API en tests et mocks/stubs.

```zsh
keploy record -c "CMD_TO_RUN_APP" 
```

Par exemple, si vous utilisez une application Python simple `CMD_TO_RUN_APP` devrait ressembler à `python main.py`, pour  Golang `go run main.go`, pour Java `java -jar xyz.jar`, pour Node.js `npm start`..

```zsh
keploy record -c "python main.py"
```

## 🧪 Lancer les tests
Arrêtez la base de données, Redis, Kafka ou tout autre service que votre application utilise. Keploy n'en n'a pas besoin durant les tests.
```zsh
keploy test -c "CMD_TO_RUN_APP" --delay 10
```

## ✅ Couverture des tests d'intégration

Pour l'intégrer avec votre bibliothèque de tests unitaires et voir la couverture de tests combinée, consultez ce [guide de couverture de tests](https://keploy.io/docs/server/sdk-installation/go/).

> ####  **Si ça vous a plu :** Vous pouvez laisser une 🌟 étoile sur ce repo ! C'est gratuit et ça nous fera sourire. 😄 👏

## Installation automatique 🚀

Configurez et lancez Keploy rapidement, aucune installation sur la machine locale n'est requise :

[![GitHub Codescape](https://img.shields.io/badge/GH%20codespace-3670A0?style=for-the-badge&logo=github&logoColor=fff)]([https://github.dev/Sonichigo/mux-sql](https://github.dev/Sonichigo/mux-sql))

## 🤔 Des questions?
N'hésitez pas à nous contacter. Nous sommes là pour vous aider !

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)


## 🌐 Langages pris en charge
Du Go's gopher 🐹 au Python's snake 🐍, nous prenons en charge :

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)
![Rust](https://img.shields.io/badge/Rust-darkred?style=for-the-badge&logo=rust&logoColor=white)
![C#](https://img.shields.io/badge/csharp-purple?style=for-the-badge&logo=csharp&logoColor=white)
![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54)

## 🫰 Ils ont adopté Keploy 🧡

Vous ou votre entreprise utilisez Keploy ? C'est génial ! Inscrivez-vous sur cette [**liste,**](https://github.com/orgs/keploy/discussions/1765) et nous vous enverrons des goodies ! 💖

Nous sommes heureux et fiers de vous avoir dans notre communauté ! 💖

## 🎩 Comment la magie opère ?

Le proxy Keploy capture et rejoue **toutes** les interactions réseau de votre application  
(opérations CRUD, y compris les API non idempotentes).

Jetez un œil à **[Comment Keploy fonctionne ?](https://keploy.io/docs/keploy-explained/how-keploy-works/)** pour découvrir l'envers du décor ! 

  ## 🔧 Fonctions clés

- ♻️ **Couverture de Tests Combinés :** Combinez vos tests Keploy avec votre bibliothèque de tests préférée (JUnit, go-test, py-test, jest) afin d’obtenir une vue combinée de la couverture des tests.


- 🤖 **Instrumentation EBPF :** Keploy utilise EBPF, la petite touche secrète pour rendre l’intégration sans code, indépendante du langage, et ultra-légère.


- 🌐 **Intégration CI/CD :** Exécutez des tests avec mocks où vous le souhaitez localement depuis le CLI, dans votre pipeline CI (Jenkins, GitHub Actions…), ou même sur un cluster Kubernetes.


- 📽️ **Capture-Rejeu de flux complexes :** Keploy peut capturer et rejouer des flux d'API distribués complexes sous forme de mocks et stubs. C'est comme avoir une machine à remonter le temps pour vos tests, un énorme gain de temps !


- 🎭 **Mocks multifonctions :** Vous pouvez aussi utiliser les mocks générés par Keploy comme tests serveur !


👉 **Découvrir le code sur GitHub**: [github.com/keploy/keploy](https://github.com/keploy/keploy)


## 👨🏻‍💻 Développons ensemble ! 👩🏻‍💻
Que vous soyez un développeur débutant ou un sorcier 🧙‍♀️, votre perspective nous est précieuse. Jetez un œil au :

📜 [Guidelines de Contribution ](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md)

❤️ [Code de Conduite ](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)


## 🐲 Limitations Actuelles !
- **Tests Unitaires :** Même si Keploy est conçu pour compléter les frameworks de tests unitaires (Go test, JUnit...) et améliorer la couverture globale, il ne génère que des tests d'intégration.
- **Environnements de Production :** Keploy est actuellement axé sur la génération de tests pour les développeurs. Ces tests peuvent être capturés depuis n'importe quel environnement, mais nous ne les avons pas testés sur des environnements de production à forte charge. Cela nécessiterait une déduplication robuste pour éviter de capturer trop de tests redondants. Néanmoins, nous avons des idées pour développer un tel système [#27](https://github.com/keploy/keploy/issues/27)

## ✨ Ressources !
🤔 [FAQs](https://keploy.io/docs/keploy-explained/faq/)

🕵️‍️ [Pourquoi Keploy](https://keploy.io/docs/keploy-explained/why-keploy/)

⚙️ [Guide d'Installation](https://keploy.io/docs/application-development/)

📖 [Guide de Contribution](https://keploy.io/docs/keploy-explained/contribution-guide/)
