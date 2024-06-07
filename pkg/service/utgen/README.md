<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="keploy logo"/>
</p>
<h3 align="center">
<b>
⚡️ Generate unit tests with LLMs, that actually works  ⚡️
</b>
</h3 >
<p align="center">
🌟 The must-have tool for developers in the AI-Gen era 🌟
</p>

---
<h4 align="center">

   <a href="https://twitter.com/Keploy_io">
    <img src="https://img.shields.io/badge/follow-%40keployio-1DA1F2?logo=twitter&style=social" alt="Keploy Twitter" />
  </a>

<a href="https://github.com/Keploy/Keploy/issues">
    <img src="https://img.shields.io/github/stars/keploy/keploy?color=%23EAC54F&logo=github&label=Help us reach 4k stars! Now at:" alt="Help us reach 4k stars!" />
  </a>

  <a href="https://landscape.cncf.io/?item=app-definition-and-development--continuous-integration-delivery--keploy">
    <img src="https://img.shields.io/badge/CNCF%20Landscape-5699C6?logo=cncf&style=social" alt="Keploy CNCF Landscape" />
  </a>


  [![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-2dno1yetd-Ec3el~tTwHYIHgGI0jPe7A)
  [![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
  [![Twitter](https://img.shields.io/badge/Twitter-%231DA1F2.svg?style=for-the-badge&logo=Twitter&logoColor=white)](https://twitter.com/Keployio)
</h4>

___

Keploy-gen uses LLMs to understand code semantics and generates meaningful unit tests. It's inspired by the [Automated Unit Test Improvement using LLM at Meta](https://arxiv.org/pdf/2402.09171).

### Objectives

- **Automate unit test generation (UTG)**: Quickly generate comprehensive unit tests and reduce the redundant manual effort.


- **Improve existing tests**: Extend and improve the scope of existing tests to cover more complex scenarios that are often missed manually.


- **Boost test coverage**: As codebase grows, ensuring exhaustive coverage should become feasible. 


## Core Components

| **Phase**                     | **Activities**                                                                                    | **Tools/Technologies**        |
|-------------------------------|---------------------------------------------------------------------------------------------------|-------------------------------|
| **Code Analysis**             | Analyze the code structure and dependencies.                                                      | Static analysis tools, LLMs   |
| **Prompt Engineering**        | Generation of targeted prompts to guide the LLM in producing relevant tests.                      | LLMs, Custom scripts          |
| **Iterative Test Refinement** | Cyclic process of refining tests by running them, analyzing coverage, and incorporating feedback. | Testing frameworks (e.g., JUnit, pytest) |

### Process Overview

![Test Refinement](https://s3.us-west-2.amazonaws.com/keploy.io/meta-llm-process-overview.png)

#### Prompt Generation
![Prompt Generation](https://s3.us-west-2.amazonaws.com/keploy.io/meta-llm-prompt-eng.png)

## Prerequisites
**AI model Setup** - Set the environment variable **export API_KEY=xxxx** to use:
  - **OpenAI's GPT-4.0** directly **[preferred]**.
  

  - Alternative LLMs via [litellm](https://github.com/BerriAI/litellm?tab=readme-ov-file#quick-start-proxy---cli).

##  Installation
Install Keploy-gen locally by running the following command:

  ```shell
  curl -O https://raw.githubusercontent.com/keploy/keploy/main/keploy.sh && source keploy.sh
  ```

### →  Setup for Node.js/TypeScript 
- Set the API key: 

  ```shell
  export API_KEY=xxxx
  ```
- Ensure Cobertura formatted coverage reports, edit `package.json`:
   ```json
   "jest": {
         "collectCoverage": true,
         "coverageReporters": ["text", "cobertura"],
         "coverageDirectory": "./coverage"
       } 
   ```
- Generate tests using Keploy:
   ```bash
   keploy gen 
          --testCommand="npm test" 
          --testDir="test" 
          --coverageReportPath="<path to coverage.xml>"
   ```


### → Setup for Golang 
- Set the API key:

  ```shell
  export API_KEY=xxxx
  ```
- Ensure Cobertura formatted coverage reports.
    ```bash
     go install github.com/axw/gocov/gocov@v1.1.0
     go install github.com/AlekSi/gocov-xml@v1.1.0 
  ```
- Generate tests using Keploy:
    ```bash
    keploy gen  
          --testDir="." 
          --testCommand="go test -v ./... -coverprofile=coverage.out && gocov convert coverage.out | gocov-xml > coverage.xml" 
          --coverageReportPath="<path to coverage.xml>"
    ```
### → Setup for Other Languages
- Set the API key:

  ```shell
  export API_KEY=xxxx
  ```
- Ensure that your unit test report format is Cobertura(it's very common).
- Generate tests using Keploy:
    ```bash
    keploy gen 
          --sourceFilePath="<path to source code file>" 
          --testFilePath="<path to existing unit test file>" 
          --testCommand="<cmd to execute unit tests>"
          --coverageReportPath="<path to cobertura-coverage.xml>"
    ```

## Configuration
Configure Keploy using command-line flags:

```bash
keploy gen 
           --sourceFilePath "" 
           --testFilePath "" 
           --coverageReportPath "coverage.xml" 
           --testCommand "" 
           --coverageFormat "cobertura" 
           --expectedCoverage 100 
           --maxIterations 5 
           --testDir "" 
           --litellmUrl "" 
           --model "gpt-4o"
```

- `sourceFilePath`: Path to the source file for which tests are to be generated.
- `testFilePath`: Path where the generated tests will be saved.
- `coverageReportPath`: Path to generate the coverage report.
- `testCommand`: Command to execute tests and generate the coverage report.
- `coverageFormat`: Type of the coverage report (default "cobertura").
- `expectedCoverage`: Desired coverage percentage (default 100%).
- `maxIterations`: Maximum number of iterations for refining tests (default 5).
- `testFolder`: Directory where tests will be written.
- `litellm`: Set to true if using litellm for model integration.
- `apiBaseUrl`: Base URL for the litellm proxy.
- `model`: Specifies the AI model to use (default "gpt-4o").

# 🙋🏻‍♀️ Questions? 🙋🏻‍♂️
Reach out to us. We're here to answer!

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-2dno1yetd-Ec3el~tTwHYIHgGI0jPe7A)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![Twitter](https://img.shields.io/badge/Twitter-%231DA1F2.svg?style=for-the-badge&logo=Twitter&logoColor=white)](https://twitter.com/Keployio)


## 🌐 Language Support
![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)

Other language may be supported, we've not tested them yet. If your **coverage reports** are of **Cobertura format** then you should be able to use keploy-gen in any language.

## Support

Keploy-gen is not just a project but an attempt to make developers life easier testing applications.
It aims to simplify the creation and maintenance of tests, ensuring high coverage, and adapts to the complexity of modern software development.

Limitation: This project currently doesn't generate quality fresh tests if there are no existing tests to learn from.  

Enjoy coding with enhanced unit test coverage! 🫰
