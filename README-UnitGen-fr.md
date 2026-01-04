<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="logo keploy"/>
</p>
<h3 align="center">
<b>
⚡️ Générez des tests unitaires avec des LLM, qui fonctionnent vraiment ⚡️
</b>
</h3 >
<p align="center">
🌟 L'outil indispensable pour les développeurs à l'ère de l'IA générative 🌟
</p>

---

<h4 align="center">

   <a href="https://x.com/keployio">
    <img src="https://img.shields.io/badge/follow-%40keployio-1DA1F2?logo=X&style=social" alt="Keploy X" />
  </a>

<a href="https://github.com/Keploy/Keploy/">
    <img src="https://img.shields.io/github/stars/keploy/keploy?color=%23EAC54F&logo=github&label=Help%20us%20reach%2020K%20stars!%20Now%20at:" alt="Aidez-nous à atteindre 20k étoiles !" />
  </a>

  <a href="https://landscape.cncf.io/?item=app-definition-and-development--continuous-integration-delivery--keploy">
    <img src="https://img.shields.io/badge/CNCF%20Landscape-5699C6?logo=cncf&style=social" alt="Paysage CNCF Keploy" />
  </a>

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/keployio)

</h4>

---

Keploy-gen utilise des LLM pour comprendre la sémantique du code et générer des **tests unitaires** pertinents. Il s'inspire de la recherche [Automated Unit Test Improvement using LLM at Meta](https://arxiv.org/pdf/2402.09171).

### Objectifs

- **Automatiser la génération de tests unitaires (UTG)** : Générez rapidement des tests unitaires complets et réduisez les efforts manuels redondants.

- **Améliorer les cas limites (Edge cases)** : Étendez et améliorez la portée des tests pour couvrir des scénarios plus complexes qui sont souvent oubliés manuellement.

- **Booster la couverture de test** : À mesure que la base de code grandit, assurer une couverture exhaustive devrait rester faisable.

## Composants Principaux

| **Phase** | **Activités** | **Outils/Technologies** |
| ----------------------------- | --------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------- |
| **Analyse de Code** | Analyser la structure du code et les dépendances. | Outils d'analyse statique, LLMs |
| **Ingénierie de Prompt** | Génération de prompts ciblés pour guider le LLM dans la production de tests pertinents. | LLMs, Scripts personnalisés |
| **Raffinement Itératif** | Processus cyclique de raffinement des tests en les exécutant, en analysant la couverture et en intégrant les retours (feedback). | Frameworks de test (ex: JUnit, pytest) |

### Aperçu du Processus

Basé sur la [recherche de Meta](https://arxiv.org/pdf/2402.09171), architecture de haut niveau TestGen-LLM.

<img src="https://s3.us-west-2.amazonaws.com/keploy.io/meta-llm-process-overview.png" width="90%" alt="Processus de raffinement des tests du générateur de tests unitaires"/>

## Prérequis

**Configuration du modèle IA** - Définissez la variable d'environnement **API_KEY**.
```
export API_KEY=xxxx
```

L'**API_KEY** peut provenir de l'une de ces sources :
- **GPT-4o d'OpenAI** directement **[préféré]**.

- LLMs alternatifs via [litellm](https://github.com/BerriAI/litellm?tab=readme-ov-file#quick-start-proxy---cli).

- Azure OpenAI

## Installation

Installez Keploy localement en exécutant la commande suivante :

#### ➡ Linux/Mac

```shell
 curl --silent -O -L [https://keploy.io/install.sh](https://keploy.io/install.sh) && source install.sh
```

#### ➡  Windows

- [Téléchargez](https://github.com/keploy/keploy/releases/latest/download/keploy_windows_amd64.tar.gz) et **déplacez le fichier keploy.exe** vers `C:\Windows\System32`

### ![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)   ➡      Exécution avec des applications Node.js/TypeScript

- Assurez-vous d'avoir défini la clé API, comme mentionné dans les prérequis ci-dessus :

  ```shell
  export API_KEY=xxxx
  ```

- Assurez-vous d'avoir des rapports de couverture au format **Cobertura**, éditez `jest.config.js` ou `package.json` :
  <br/>

  ```json
  // package.json
  "jest": {
        "coverageReporters": ["text", "cobertura"],
      }
  ```
  ou  

  ```javascript
    // jest.config.js
    module.exports = {
      coverageReporters: ["text", "cobertura"],
    };
  ```

#### Génération de Tests Unitaires

- Exécutez la commande suivante à la racine de votre application. 
  <br/>

  - **Pour un seul fichier de test :** Si vous préférez tester une plus petite section de votre application ou contrôler les coûts, envisagez de générer des tests pour une seule source et son fichier de test correspondant :

    ```shell
     keploy gen --sourceFilePath="<chemin vers le fichier source>" --testFilePath="<chemin vers le fichier de test pour la source ci-dessus>" --testCommand="npm test" --coverageReportPath="<chemin vers coverage.xml>"
    ```

    <br/>

  - **Pour toute l'application**, utilisez la commande suivante pour générer des tests sur l'ensemble :

    ⚠️ **Avertissement :** L'exécution de cette commande générera des tests unitaires pour tous les fichiers de l'application. Selon la taille de la base de code, ce processus peut prendre entre 20 minutes et une heure et entraînera des coûts liés à l'utilisation du LLM.

    ```bash
    keploy gen --testCommand="npm test" --testDir="test" --coverageReportPath="<chemin vers coverage.xml>"
    ```

  🎉 Vous devriez voir des cas de test améliorés et une meilleure couverture de code. ✅ Profitez du codage avec une couverture de test unitaire améliorée ! 🫰

### ![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white) → Exécution avec des applications Golang

- Assurez-vous d'avoir défini la clé API, comme mentionné dans les prérequis ci-dessus :

  ```shell
  export API_KEY=xxxx
  ```

- Pour garantir des rapports de couverture au format **Cobertura**, ajoutez :
  ```bash
   go install [github.com/axw/gocov/gocov@v1.1.0](https://github.com/axw/gocov/gocov@v1.1.0)
   go install [github.com/AlekSi/gocov-xml@v1.1.0](https://github.com/AlekSi/gocov-xml@v1.1.0)
  ```
#### Génération de Tests Unitaires
- Exécutez la commande suivante à la racine de votre application.
  <br/>

  - **Pour un seul fichier de test :** Si vous préférez tester une plus petite section de votre application ou contrôler les coûts, envisagez de générer des tests pour une seule source et son fichier de test correspondant :

    ```shell
    keploy gen --sourceFilePath="<chemin vers le fichier source>" --testFilePath="<chemin vers le fichier de test pour la source ci-dessus>" --testCommand="go test -v ./... -coverprofile=coverage.out && gocov convert coverage.out | gocov-xml > coverage.xml" --coverageReportPath="<chemin vers coverage.xml>"
    ```

    <br/>

  - **Pour toute l'application**, utilisez la commande suivante pour générer des tests sur l'ensemble :

    ⚠️ **Avertissement :** L'exécution de cette commande générera des tests unitaires pour tous les fichiers de l'application. Selon la taille de la base de code, ce processus peut prendre entre 20 minutes et une heure et entraînera des coûts liés à l'utilisation du LLM.

    ```bash
    keploy gen --testDir="." --testCommand="go test -v ./... -coverprofile=coverage.out && gocov convert coverage.out | gocov-xml > coverage.xml" --coverageReportPath="<chemin vers coverage.xml>"
    ```

    🎉 Vous devriez voir des cas de test améliorés et une meilleure couverture de code. ✅ Profitez du codage avec une couverture de test unitaire améliorée ! 🫰

### → Configuration pour d'autres langages

- Assurez-vous d'avoir défini la clé API, comme mentionné dans les prérequis ci-dessus :

  ```shell
  export API_KEY=xxxx
  ```

- Assurez-vous que le format de votre rapport de test unitaire est **Cobertura** (c'est très courant).
- Générez des tests en utilisant keploy-gen :
  ```bash
  keploy gen --sourceFilePath="<chemin vers le fichier de code source>" --testFilePath="<chemin vers le fichier de test unitaire existant>" --testCommand="<cmd pour exécuter les tests unitaires>" --coverageReportPath="<chemin vers cobertura-coverage.xml>"
  ```

## Configuration

Configurez Keploy en utilisant les drapeaux (flags) de ligne de commande :

```bash

  --sourceFilePath ""
  --testFilePath ""
  --coverageReportPath "coverage.xml"
  --testCommand ""
  --coverageFormat "cobertura"
  --expectedCoverage 100
  --maxIterations 5
  --testDir ""
  --llmBaseUrl "[https://api.openai.com/v1](https://api.openai.com/v1)"
  --model "gpt-4o"
  --llmApiVersion "
```

- `sourceFilePath`: Chemin vers le fichier source pour lequel les tests doivent être générés.
- `testFilePath`: Chemin où les tests générés seront enregistrés.
- `coverageReportPath`: Chemin pour générer le rapport de couverture.
- `testCommand` (requis): Commande pour exécuter les tests et générer le rapport de couverture.
- `coverageFormat`: Type du rapport de couverture (par défaut "cobertura").
- `expectedCoverage`: Pourcentage de couverture souhaité (par défaut 100%).
- `maxIterations`: Nombre maximum d'itérations pour affiner les tests (par défaut 5).
- `testDir`: Répertoire où les tests seront écrits.
- `llmBaseUrl`: URL de base du LLM.
- `model`: Spécifie le modèle IA à utiliser (par défaut "gpt-4o").
- `llmApiVersion`: Version de l'API du LLM le cas échéant (par défaut "")

# Foire Aux Questions (FAQ)

1. Qu'est-ce que le Générateur de Tests Unitaires (UTG) de Keploy ? <br>
    - L'UTG de Keploy automatise la création de tests unitaires basés sur la sémantique du code, améliorant ainsi la couverture et la fiabilité des tests.

2. Keploy envoie-t-il vos données privées à un serveur cloud pour la génération de tests ?<br>
    - Non, Keploy n'envoie aucun code utilisateur à des systèmes distants, sauf lors de l'utilisation de la fonctionnalité de génération de tests unitaires. Lors de l'utilisation de la fonctionnalité UT gen, seuls le code source et le code de test unitaire seront envoyés au Grand Modèle de Langage (LLM) que vous utilisez. Par défaut, Keploy utilise - litellm pour supporter un grand nombre de backends LLM. Oui, si votre organisation possède son propre LLM (privé), vous pouvez l'utiliser avec Keploy. Cela garantit que les données ne sont envoyées à aucun système externe.

3. Comment Keploy contribue-t-il à améliorer la couverture des tests unitaires ?<br>
    - En fournissant une plateforme sans code (zero code) pour les tests automatisés, Keploy permet aux développeurs d'augmenter leur couverture de tests unitaires sans connaissances approfondies en codage. Cette intégration améliore les rapports de test, renforçant finalement la confiance dans la qualité du produit.

4. Keploy est-il rentable pour les tests unitaires automatisés ?<br>
   - Oui, Keploy optimise les coûts en automatisant les tâches de test répétitives et en améliorant l'efficacité globale des tests.

5. Comment Keploy génère-t-il les rapports de couverture ?<br>
    - Keploy génère des rapports détaillés au format Cobertura, offrant des informations sur l'efficacité des tests et la qualité du code.

6. Keploy peut-il gérer efficacement de grandes bases de code ?<br>
   - Oui, Keploy est conçu pour gérer efficacement de grandes bases de code, bien que le temps de traitement puisse varier en fonction de la taille et de la complexité du projet.

# 🙋🏻‍♀️ Des Questions ? 🙋🏻‍♂️

Contactez-nous. Nous sommes là pour répondre !

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)


# 📝 Exemples de Démarrage Rapide
- ![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white) : Essayez unit-gen sur l'application [Mux-SQL](https://github.com/keploy/samples-go/tree/main/mux-sql#create-unit-testcase-with-keploy)

- ![Node](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node&logoColor=white) : Essayez unit-gen sur l'application [Express-Mongoose](https://github.com/keploy/samples-typescript/tree/main/express-mongoose#create-unit-testcase-with-keploy)

## 🌐 Support des Langages

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)

D'autres langages peuvent être supportés, nous ne les avons pas encore testés. Si vos **rapports de couverture** sont au **format Cobertura**, alors vous devriez pouvoir utiliser keploy-gen dans n'importe quel langage.

## Support Développeur

Keploy-gen n'est pas seulement un projet mais une tentative de faciliter la vie des développeurs testant des applications.
Il vise à simplifier la création et la maintenance des tests, assurant une couverture élevée, et s'adapte à la complexité du développement logiciel moderne.

#### Génération de Prompt

Basé sur la [recherche de Meta](https://arxiv.org/pdf/2402.09171), les quatre principaux prompts utilisés lors du déploiement pour les test-a-thons des applications Instagram et Facebook de décembre 2023.

| Nom du Prompt         | Modèle (Template)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| --------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| extend_test           | Here is a Kotlin unit test class: {`existing_test_class`}. Write an extended version of the test class that includes additional tests to cover some extra corner cases.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| extend_coverage       | Here is a Kotlin unit test class and the class that it tests: {`existing_test_class`} {`class_under_test`}. Write an extended version of the test class that includes additional unit tests that will increase the test coverage of the class under test.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| corner_cases          | Here is a Kotlin unit test class and the class that it tests: {`existing_test_class`} {`class_under_test`}. Write an extended version of the test class that includes additional unit tests that will cover corner cases missed by the original and will increase the test coverage of the class under test.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| statement_to_complete | Here is a Kotlin class under test {`class_under_test`} This class under test can be tested with this Kotlin unit test class {`existing_test_class`}. Here is an extended version of the unit test class that includes additional unit test cases that will cover methods, edge cases, corner cases, and other features of the class under test that were missed by the original unit test class: |

Limitation : Ce projet ne génère actuellement pas de tests frais de qualité s'il n'y a pas de tests existants dont il peut apprendre.

Profitez du codage avec une couverture de test unitaire améliorée ! 🫰