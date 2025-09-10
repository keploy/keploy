<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="logo keploy"/>
</p>
<h3 align="center">
<b>
⚡️ Tests d'API plus rapides que les tests unitaires, à partir du trafic utilisateur ⚡️
</b>
</h3 >
<p align="center">
🌟 L'outil indispensable pour les développeurs à l'ère de l'IA-Génération 🌟
</p>

---

<h4 align="center">

<a href="https://x.com/Keployio">
    <img src="https://img.shields.io/badge/follow-%40keployio-1DA1F2?logo=X&style=social" alt="Keploy X!" />
  </a>

<a href="https://github.com/Keploy/Keploy/">
   <img src="https://img.shields.io/github/stars/keploy/keploy?color=%23EAC54F&logo=github&label=Help%20us%20reach%2020K%20stars!%20Now%20at:" alt="Aidez-nous à atteindre 20k étoiles ! Actuellement à :" />
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

[Keploy](https://keploy.io) est un outil de test d'API **centré sur les développeurs** qui crée **des tests avec des mocks intégrés**, plus rapidement que les tests unitaires.

Keploy enregistre non seulement les appels d'API, mais aussi les appels à la base de données et les rejoue pendant les tests, le rendant **facile à utiliser, puissant et extensible**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="Convert API calls to test cases"/>

> 🐰 **Fait amusant :** Keploy s'utilise lui-même pour les tests ! Découvrez notre badge de couverture élégant : [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

## 🚨 Vous êtes ici pour le [Générateur de tests unitaires](README-UnitGen.md) (ut-gen) ?

Keploy vient de lancer la première implémentation mondiale d'un générateur de tests unitaires (ut-gen) basée sur le [document de recherche Meta LLM](https://arxiv.org/pdf/2402.09171). Il comprend la sémantique du code et génère des tests unitaires pertinents, avec pour objectifs :

- **Automatiser la génération de tests unitaires (UTG)** : Générer rapidement des tests unitaires complets et réduire les efforts manuels redondants.

- **Améliorer les cas limites** : Étendre et améliorer la portée des tests automatisés pour couvrir des scénarios plus complexes, souvent oubliés manuellement.

- **Augmenter la couverture des tests** : À mesure que les bases de code grandissent, assurer une couverture exhaustive devrait devenir réalisable, conformément à notre mission.

### 📜 Suivez le [README du Générateur de tests unitaires](README-UnitGen.md) ! ✅

## 📘 Documentation !

Devenez un expert Keploy avec la **[Documentation Keploy](https://keploy.io/docs/)**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="Enregistrement et relecture des tests"/>

# 🚀 Installation rapide (Générateur de tests API)

Intégrez Keploy en installant l'agent localement. Aucune modification de code requise.

```shell
curl --silent -O -L https://keploy.io/install.sh && source install.sh
```

## 🎬 Enregistrement des cas de test

Démarrez votre application avec Keploy pour convertir les appels API en tests et simulations (mocks/stubs).

```zsh
keploy record -c "CMD_TO_RUN_APP" 
```

Par exemple, si vous utilisez une application Python simple, la `CMD_TO_RUN_APP` ressemblera à `python main.py`, pour Golang `go run main.go`, pour Java `java -jar xyz.jar`, pour Node `npm start`..

```zsh
keploy record -c "python main.py"
```

## 🧪 Exécution des tests

Arrêtez les bases de données, Redis, Kafka ou tout autre service utilisé par votre application. Keploy n'en a pas besoin pendant les tests.

```zsh
keploy test -c "CMD_TO_RUN_APP" --delay 10
```

## ✅ Intégration de la couverture de test

Pour intégrer avec votre bibliothèque de tests unitaires et voir la couverture combinée, suivez ce [guide de couverture de test](https://keploy.io/docs/server/sdk-installation/go/).

> ####  **Si vous vous êtes amusé·e :** Laissez une étoile 🌟 sur ce dépôt ! C'est gratuit et ça fera plaisir. 😄 👏

## Configuration en un clic 🚀

Configurez et exécutez Keploy rapidement, sans installation requise sur votre machine locale :

[![GitHub Codescape](https://img.shields.io/badge/GH%20codespace-3670A0?style=for-the-badge&logo=github&logoColor=fff)]([https://github.dev/Sonichigo/mux-sql](https://github.dev/Sonichigo/mux-sql))

## 🤔 Des questions ?

Contactez-nous. Nous sommes là pour vous aider !

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)

## 🌐 Support des Langages

Du gopher de Go 🐹 au serpent de Python 🐍, nous prenons en charge :

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)
![Rust](https://img.shields.io/badge/Rust-darkred?style=for-the-badge&logo=rust&logoColor=white)
![C#](https://img.shields.io/badge/csharp-purple?style=for-the-badge&logo=csharp&logoColor=white)
![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54)

## 🫰 Adoptants de Keploy 🧡

Alors, vous et votre organisation utilisez Keploy ? C'est génial. Ajoutez-vous à [**cette liste,**](https://github.com/orgs/keploy/discussions/1765) et nous vous enverrons des goodies ! 💖

Nous sommes heureux et fiers de vous compter parmi notre communauté ! 💖

## 🎩 Comment la magie opère-t-elle ?

Le proxy Keploy capture et rejoue **TOUTES** les interactions réseau de votre application (opérations CRUD, y compris les API non idempotentes).

Partez à la découverte des coulisses avec **[Comment fonctionne Keploy ?](https://keploy.io/docs/keploy-explained/how-keploy-works/)** !

## 🔧 Fonctionnalités principales

- ♻️ **Couverture de test combinée :** Fusionnez vos tests Keploy avec vos bibliothèques de test préférées (JUnit, go-test, py-test, jest) pour obtenir une couverture de test globale.  

- 🤖 **Instrumentation EBPF :** Keploy utilise EBPF comme ingrédient secret pour rendre l'intégration sans code, indépendante du langage et ultra-légère.  

- 🌐 **Intégration CI/CD :** Exécutez des tests avec des mocks où vous voulez—localement en CLI, dans votre pipeline CI (Jenkins, Github Actions...), ou même sur un cluster Kubernetes.  

- 📽️ **Enregistrement et relecture de flux complexes :** Keploy peut enregistrer et rejouer des flux API distribués complexes sous forme de mocks et stubs. C'est comme une machine à remonter le temps pour vos tests—un gain de temps considérable !  

- 🎭 **Mocks polyvalents :** Vous pouvez aussi utiliser les Mocks générés par Keploy comme tests serveur !

👉 **Explorez le code sur GitHub :** [github.com/keploy/keploy](https://github.com/keploy/keploy)

## 👨🏻‍💻 Construisons ensemble ! 👩🏻‍💻

Que vous soyez un·e développeur·se débutant·e ou un·e expert·e �, votre perspective est précieuse. Jetez un œil à nos :

📜 [Lignes directrices pour contribuer](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md)

❤️ [Code de conduite](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)

## 🐲 Limitations actuelles !

- **Tests unitaires :** Bien que Keploy soit conçu pour fonctionner avec des frameworks de tests unitaires (Go test, JUnit...) et puisse augmenter la couverture de code globale, il génère toujours des tests d'intégration.
- **Environnements de production :** Keploy se concentre actuellement sur la génération de tests pour les développeurs. Ces tests peuvent être capturés depuis n'importe quel environnement, mais nous ne l'avons pas testé sur des environnements de production à fort trafic. Cela nécessiterait un système robuste de déduplication pour éviter de capturer trop de tests redondants. Nous avons des idées pour construire un tel système [#27](https://github.com/keploy/keploy/issues/27)

## ✨ Ressources !

🤔 [FAQ](https://keploy.io/docs/keploy-explained/faq/)

🕵️‍ [Pourquoi Keploy](https://keploy.io/docs/keploy-explained/why-keploy/)

⚙️ [Guide d'installation](https://keploy.io/docs/application-development/)

📖 [Guide de contribution](https://keploy.io/docs/keploy-explained/contribution-guide/)