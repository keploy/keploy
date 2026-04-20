<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="keploy logo"/>
</p>
<h3 align="center">
<b>
⚡️ Tests d'API plus rapides que les tests unitaires, basés sur le trafic utilisateur  ⚡️
</b>
</h3 >
<p align="center">
🌟 L'outil incontournable pour les développeurs à l'ère de l'IA 🌟
</p>

---

<h4 align="center">

   <a href="https://x.com/keployio">
    <img src="https://img.shields.io/badge/follow-%40keployio-1DA1F2?logo=X&style=social" alt="Keploy X" />
  </a>

<a href="https://github.com/Keploy/Keploy/">
    <img src="https://img.shields.io/github/stars/keploy/keploy?color=%23EAC54F&logo=github&label=Help%20us%20reach%2020K%20stars!%20Now%20at:" alt="Help us reach 20k stars!" />
  </a>

  <a href="https://landscape.cncf.io/?item=app-definition-and-development--continuous-integration-delivery--keploy">
    <img src="https://img.shields.io/badge/CNCF%20Landscape-5699C6?logo=cncf&style=social" alt="Keploy CNCF Landscape" />
  </a>

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/keployio)

</h4>

---

Keploy-gen utilise les LLM pour comprendre la sémantique du code et générer des **tests unitaires** pertinent. Il est inspiré par le [Automated Unit Test Improvement using LLM at Meta](https://arxiv.org/pdf/2402.09171).

### Objectifs

- **Génération automatique des tests unitaires** (UTG) : Générer rapidement des tests unitaires complets et réduire les efforts manuels redondants.

- **Améliorer la couverture des cas limites** : Étendre et améliorer la portée des tests pour couvrir des scénarios complexes souvent négligés lors des tests manuels.

- **Augmenter la couverture de tests** : À mesure que la base de code grandit, assurer une couverture exhaustive devrait devenir réalisable.

## Composants principaux

| **Phase**                     | **Activitiés**                                                                                    | **Outils/Technologies**                   |
| ----------------------------- | ------------------------------------------------------------------------------------------------- | ---------------------------------------- |
| **Analyse du Code**             | Analyse de la structure du code et des dépendances  | Outils d'analyse statique, les LLM              |
| **Conception de prompts**        | Génération de prompts ciblés pour guider le LLM dans la production de tests pertinents.                       | Scripts personnalisés, les LLM
| **Optimisation itérative des tests** | Processus cyclique de l'amélioration des tests en les exécutants, en analysant la couverture et en intégrant les retours. | Frameworks de Test (ex : JUnit, pytest) |

### Aperçu du processus

En référence au [Meta's research](https://arxiv.org/pdf/2402.09171), architecture de haut niveau de TestGen-LLM.

<img src="https://s3.us-west-2.amazonaws.com/keploy.io/meta-llm-process-overview.png" width="90%" alt="Test refinement process of unit test generator"/>

## Prérequis

**Configuration du modèle IA** - Définir la variable d'environnement **API_KEY**.
```
export API_KEY=xxxx
```

La **API_KEY** peut être trouvé ici : 
- **GPT-4o d'OpenAI**  [préféré]

- LLMs alternatifs via [litellm](https://github.com/BerriAI/litellm?tab=readme-ov-file#quick-start-proxy---cli).

- Azure OpenAI

## Installation

Installer Keploy localement en exécutant la commande suivante :

#### ➡ Linux/Mac

```shell
 curl --silent -O -L https://keploy.io/install.sh && source install.sh
```

#### ➡  Windows

- [Télécharger](https://github.com/keploy/keploy/releases/latest/download/keploy_windows_amd64.tar.gz) et **déplacer le fichier keploy.exe** vers `C:\Windows\System32`

### ![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)   ➡  Exécuter l'application avec Node.js/TypeScript

- Assurez-vous que la clé API a bien été définie comme mentionné dans les prérequis ci-dessus : 

  ```shell
  export API_KEY=xxxx
  ```

- Assurez-vous que les rapports de couverture au format **Cobertura**  sont générés en modifiant le `jest.config.js` ou `package.json`:
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

#### Génération des tests unitaires

- Exécutez la commande suivante à la racine de votre application.
  <br/>

  - **Pour un fichier de test individuel :** Si vous préférez tester une petite section de votre application ou contrôler les coûts, envisagez de générer des tests pour un seul fichier source et son fichier de test correspondant :

    ```shell
     keploy gen --sourceFilePath="<chemin du fichier source>" --testFilePath="<chemin vers le fichier de test pour le fichier source>" --testCommand="npm test" --coverageReportPath="<chemin du fichier rapport de couverture.xml>"
    ```

    <br/>

  - **Pour l'ensemble de l'application** utilisez la commande suivante pour générer des tests :

    ⚠️ **Avertissement :** L'exécution de cette commande générera des tests unitaires pour tous les fichiers de l'application. Selon la taille de la base de code, ce processus peut prendre entre 20 minutes et une heure et entraînera des coûts liés à l'utilisation des LLM.

    ```bash
    keploy gen --testCommand="npm test" --testDir="test" --coverageReportPath="<chemin du fichier rapport de couverture.xml>"
    ```

    🎉 Vous devriez constater une amélioration des cas de tests et de la couverture de code. ✅ Profitez bien du développement avec une couverture de tests unitaires améliorée ! 🫰

### ![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white) → Exécuter avec une application sous Golang

- Assurez-vous que la clé API a bien été définie comme mentionné dans les prérequis ci-dessus :

  ```shell
  export API_KEY=xxxx
  ```

- Pour garantir des rapports de couverture au format Cobertura, ajoutez : 
  ```bash
   go install github.com/axw/gocov/gocov@v1.1.0
   go install github.com/AlekSi/gocov-xml@v1.1.0
  ```
#### Génération des tests unitaires
- Exécutez la commande suivante à la racine de votre application.
  <br/>

  - **Pour un fichier de test individuel :** Si vous préférez tester une petite section de votre application ou contrôler les coûts, envisagez de générer des tests pour un seul fichier source et son fichier de test correspondant :

    ```shell
    keploy gen --sourceFilePath="<chemin du fichier source>" --testFilePath="<chemin vers le fichier de test pour le fichier source>" --testCommand="go test -v ./... -coverprofile=coverage.out && gocov convert coverage.out | gocov-xml > coverage.xml" --coverageReportPath="<chemin du fichier rapport de couverture.xml>"
    ```

    <br/>

  - **Pour l'ensemble de l'application** utilisez la commande suivante pour générer des tests :

    ⚠️ **Avertissement:** Exécuter cette commande générera des tests unitaires pour tous les fichiers de l'application. En fonction de la taille de la base de code, le processus pourrait prendre entre 20 minutes et une heure, entraînant des coûts liés à l'utilisation du LLM.

    ```bash
    keploy gen --testDir="." --testCommand="go test -v ./... -coverprofile=coverage.out && gocov convert coverage.out | gocov-xml > coverage.xml" --coverageReportPath="<chemin du fichier rapport de couverture.xml>"
    ```

    🎉 Vous devriez constater une amélioration des cas de tests et de la couverture de code. ✅ Profitez bien du développement avec une couverture de tests unitaires améliorée ! 🫰

### → Configuration pour les autres langages

- Assurez-vous que la clé API a bien été définie comme mentionné dans les prérequis ci-dessus :

  ```shell
  export API_KEY=xxxx
  ```

- Assurez-vous que le format de rapport de test unitaire soit Cobertura (chose très commune).
- Générer les tests en utilisant keploy-gen :
  ```bash
  keploy gen --sourceFilePath="chemin vers le fichier de test pour le fichier source>" --testFilePath="<chemin pour le fichier de test unitaire existant>" --testCommand="<cmd pour exécuter les tests unitaires>" --coverageReportPath="<chemin du fichier rapport de couverture.xml>"
  ```

## Configuration
Configurez Keploy avec les arguments de ligne de commande

```bash

  --sourceFilePath ""
  --testFilePath ""
  --coverageReportPath "coverage.xml"
  --testCommand ""
  --coverageFormat "cobertura"
  --expectedCoverage 100
  --maxIterations 5
  --testDir ""
  --llmBaseUrl "https://api.openai.com/v1"
  --model "gpt-4o"
  --llmApiVersion "
```

- `sourceFilePath`: Chemin vers le fichier source pour lequel les tests doivent être générés.
- `testFilePath`: Chemin où les tests générés seront sauvegardés.
- `coverageReportPath`: Chemin pour générer le rapport de couverture.
- `testCommand` (requis) : Commande pour exécuter les tests et générer le rapport de couverture.
- `coverageFormat`: Type de rapport de couverture (par défaut "cobertura").
- `expectedCoverage`: Pourcentage de couverture souhaité (par défaut 100%).
- `maxIterations`: Nombre maximum d'itérations pour affiner les tests (par défaut 5).
- `testDir`: Répertoire où les tests seront écrits.
- `llmBaseUrl`: URL de base du LLM.
- `model`: Spécifie le modèle IA à utiliser (par défaut "gpt-4o").
- `llmApiVersion`: Version de l'API du LLM si applicable (par défaut "").

# Questions les plus fréquentes

1. Qu'est-ce que le générateur de tests unitaires (GTU) de Keploy ? <br>
    - Le GTU de Keploy automatise la création de tests unitaires basés sur la sémantique du code, améliorant la couverture de tests et la fiabilité.

2. Keploy envoie-t-il vos données privées vers un serveur cloud pour la génération de tests ?<br>
    - Non, Keploy n’envoie aucun code utilisateur vers des systèmes distants, sauf lors de l’utilisation de la fonctionnalité de génération de tests unitaires. Lorsqu’on utilise cette fonctionnalité (UT gen), seuls le code source et le code du test unitaire sont envoyés au modèle de langage (LLM) que vous utilisez. Par défaut, Keploy utilise litellm pour prendre en charge un grand nombre de backends LLM. Oui, si votre organisation dispose de son propre LLM (privé), vous pouvez l’utiliser avec Keploy. Cela garantit que les données ne sont pas envoyées vers des systèmes externes.

3. Comment Keploy contribue-t-il à l'amélioration de la couverture des tests unitaires ?<br>
    - En proposant une plateforme no code pour les tests automatisés, Keploy encourage les développeurs à augmenter leur couverture de tests unitaires sans avoir besoin de connaissances approfondies en développement. Cette intégration améliore les rapports de test, améliorant finalement la confiance dans la qualité du produit.

4. Keploy offre-t-il un bon rapport coût-efficacité pour l'automatisation des tests unitaires ?<br>
   - Oui, Keploy optimise les coûts en automatisant les tâches de test répétitives et en améliorant l'efficacité globale des tests.

5. Comment Keploy génère-t-il les rapports de couverture de tests ?<br>
    - Keploy génère des rapports détaillés au format Cobertura, offrant des informations sur l'efficacité des tests et la qualité du code.

6. Est-ce que Keploy peut gérer efficacement de grandes bases de code ?<br>
   - Oui, Keploy est conçu pour gérer efficacement de grandes bases de code, bien que le temps de traitement puisse varier en fonction de la taille et de la complexité du projet.

# 🙋🏻‍♀️ Des questions? 🙋🏻‍♂️

N'hésitez pas à nous contacter, nous sommes là pour répondre à vos questions !

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)

# 📝 Exemples de guides de démarrage

- ![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white) : Essayez un unit-gen sur une application [Mux-SQL](https://github.com/keploy/samples-go/tree/main/mux-sql#create-unit-testcase-with-keploy)

- ![Node](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node&logoColor=white) : Essayez un unit-gen sur une application [Express-Mongoose](https://github.com/keploy/samples-typescript/tree/main/express-mongoose#create-unit-testcase-with-keploy)

## 🌐  Langages pris en charge

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)

D'autres langages pourraient être potentiellement pris en charge, nous ne les avons pas encore testés. Si vos **rapports de couverture** sont au **format Cobertura**, vous devriez pouvoir utiliser keploy-gen dans n'importe quel langage.

## Support pour les développeurs

Keploy-gen n'est pas juste un projet mais une tentative de simplifier la vie des développeurs qui testent leurs applications. Il vise à simplifier la création et la maintenance des tests, en assurant une couverture élevée, et s'adapte à la complexité du développement logiciel moderne.

#### Génération de Prompt

En référence à la [Meta's research](https://arxiv.org/pdf/2402.09171), les quatre prompts principaux utilisés lors du déploiement des test-a-thons des applications Instagram et Facebook en décembre 2023.

| Nom du Prompt           | Template                                                                                                                                                                                                                                                                                                                                                                                         |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| extend_test           | Voici une classe de test unitaire Kotlin : {`class_test_existante`}. Écrivez une version étendue de cette classe de test qui inclut des tests supplémentaires pour couvrir quelques cas limites supplémentaires.                                                    |
| extend_coverage       | Voici une classe de test unitaire Kotlin et la classe qu'elle teste : {`class_test_existante`} {`class_sous_test`}. Écrivez une version étendue de la classe de test qui inclut des tests unitaires supplémentaires permettant d'augmenter la couverture de test de la classe testée.                                                                                                                                        |
| corner_cases          | Voici une classe de test unitaire Kotlin et la classe qu'elle teste : {`class_test_existante`} {`class_sous_test`}. Écrivez une version étendue de la classe de test qui inclut des tests unitaires supplémentaires couvrant les cas limites manqués par l'original et qui augmenteront la couverture de test de la classe testée.                                                                                     |
| statement_to_complete | Voici une classe Kotlin à tester {`class_sous_test`}. Cette classe peut être testée avec cette classe de test unitaire Kotlin {`class_test_existante`}. Voici une version étendue de la classe de test unitaire qui inclut des cas de test unitaires supplémentaires couvrant les méthodes, les cas limites, les cas particuliers, et autres fonctionnalités de la classe testée qui ont été manqués par la classe de test unitaire originale : |

Limitation : Ce projet ne génère pas encore des tests de qualité s'il n'existe pas de tests existants pour apprendre.

Profitez de développer avec une couverture de tests unitaires améliorée ! 🫰 🫰
