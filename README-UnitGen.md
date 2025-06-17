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

Keploy-gen uses LLMs to understand code semantics and generates meaningful **unit tests**. It's inspired by the [Automated Unit Test Improvement using LLM at Meta](https://arxiv.org/pdf/2402.09171).

### Objectives

- **Automate unit test generation (UTG)**: Quickly generate comprehensive unit tests and reduce the redundant manual effort.

- **Improve edge cases**: Extend and improve the scope of tests to cover more complex scenarios that are often missed manually.

- **Boost test coverage**: As codebase grows, ensuring exhaustive coverage should become feasible.

## Core Components

| **Phase**                     | **Activities**                                                                                    | **Tools/Technologies**                   |
| ----------------------------- | ------------------------------------------------------------------------------------------------- | ---------------------------------------- |
| **Code Analysis**             | Analyze the code structure and dependencies.                                                      | Static analysis tools, LLMs              |
| **Prompt Engineering**        | Generation of targeted prompts to guide the LLM in producing relevant tests.                      | LLMs, Custom scripts                     |
| **Iterative Test Refinement** | Cyclic process of refining tests by running them, analyzing coverage, and incorporating feedback. | Testing frameworks (e.g., JUnit, pytest) |

### Process Overview

Referred from [Meta's research](https://arxiv.org/pdf/2402.09171), TestGen-LLM top level architecture.

<img src="https://s3.us-west-2.amazonaws.com/keploy.io/meta-llm-process-overview.png" width="90%" alt="Test refinement process of unit test generator"/>

## Prerequisites

**AI model Setup** - Set the environment variable **API_KEY**.
```
export API_KEY=xxxx
```

**API_KEY** can be from either of one these:
- **OpenAI's GPT-4o** directly **[preferred]**.

- Alternative LLMs via [litellm](https://github.com/BerriAI/litellm?tab=readme-ov-file#quick-start-proxy---cli).

- Azure OpenAI

## Installation

Install Keploy locally by running the following command:

#### ➡ Linux/Mac

```shell
 curl --silent -O -L https://keploy.io/install.sh && source install.sh
```

#### ➡  Windows

- [Download](https://github.com/keploy/keploy/releases/latest/download/keploy_windows_amd64.tar.gz) and **move the keploy.exe file** to `C:\Windows\System32`

### ![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)   ➡     Running with Node.js/TypeScript applications

- Ensure you've set the API key, as mentioned in pre-requisites above:

  ```shell
  export API_KEY=xxxx
  ```

- Ensure **Cobertura** formatted coverage reports, edit `jest.config.js` or `package.json`:
  <br/>

  ```json
  // package.json
  "jest": {
        "coverageReporters": ["text", "cobertura"],
      }
  ```
  or  

  ```javascript
    // jest.config.js
    module.exports = {
      coverageReporters: ["text", "cobertura"],
    };
  ```

#### Generating Unit Tests

- Run the following command in the root of your application. 
  <br/>

  - **For Single Test File:** If you prefer to test a smaller section of your application or to control costs, consider generating tests for a single source and its corresponding test file:

    ```shell
     keploy gen --sourceFilePath="<path to source file>" --testFilePath="<path to test file for above source file>" --testCommand="npm test" --coverageReportPath="<path to coverage.xml>"
    ```

    <br/>

  - **For Entire Application** use the following command to generate tests across:

    ⚠️ **Warning:** Executing this command will generate unit tests for all files in the application. Depending on the size of the codebase, this process may take between 20 minutes to an hour and will incur costs related to LLM usage.

    ```bash
    keploy gen --testCommand="npm test" --testDir="test" --coverageReportPath="<path to coverage.xml>"
    ```

  🎉 You should see improved test cases and code-coverage. ✅ Enjoy coding with enhanced unit test coverage! 🫰

### ![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white) → Running with Golang applications

- Ensure you've set the API key, as mentioned in pre-requisites above:

  ```shell
  export API_KEY=xxxx
  ```

- To ensure **Cobertura** formatted coverage reports, add:
  ```bash
   go install github.com/axw/gocov/gocov@v1.1.0
   go install github.com/AlekSi/gocov-xml@v1.1.0
  ```
#### Generating Unit Tests
- Run the following command in the root of your application.
  <br/>

  - **For Single Test File:** If you prefer to test a smaller section of your application or to control costs, consider generating tests for a single source and its corresponding test file:

    ```shell
    keploy gen --sourceFilePath="<path to source file>" --testFilePath="<path to test file for above source file>" --testCommand="go test -v ./... -coverprofile=coverage.out && gocov convert coverage.out | gocov-xml > coverage.xml" --coverageReportPath="<path to coverage.xml>"
    ```

    <br/>

  - **For Entire Application** use the following command to generate tests across:

    ⚠️ **Warning:** Executing this command will generate unit tests for all files in the application. Depending on the size of the codebase, this process may take between 20 minutes to an hour and will incur costs related to LLM usage.

    ```bash
    keploy gen --testDir="." --testCommand="go test -v ./... -coverprofile=coverage.out && gocov convert coverage.out | gocov-xml > coverage.xml" --coverageReportPath="<path to coverage.xml>"
    ```

    🎉 You should see improved test cases and code-coverage. ✅ Enjoy coding with enhanced unit test coverage! 🫰

### → Setup for Other Languages

- Ensure you've set the API key, as mentioned in pre-requisites above:

  ```shell
  export API_KEY=xxxx
  ```

- Ensure that your unit test report format is **Cobertura**(it's very common).
- Generate tests using keploy-gen:
  ```bash
  keploy gen --sourceFilePath="<path to source code file>" --testFilePath="<path to existing unit test file>" --testCommand="<cmd to execute unit tests>" --coverageReportPath="<path to cobertura-coverage.xml>"
  ```

## Configuration

Configure Keploy using command-line flags:

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

- `sourceFilePath`: Path to the source file for which tests are to be generated.
- `testFilePath`: Path where the generated tests will be saved.
- `coverageReportPath`: Path to generate the coverage report.
- `testCommand` (required): Command to execute tests and generate the coverage report.
- `coverageFormat`: Type of the coverage report (default "cobertura").
- `expectedCoverage`: Desired coverage percentage (default 100%).
- `maxIterations`: Maximum number of iterations for refining tests (default 5).
- `testDir`: Directory where tests will be written.
- `llmBaseUrl`: Base url of the llm.
- `model`: Specifies the AI model to use (default "gpt-4o").
- `llmApiVersion`: API version of the llm if any (default "")

# Frequently Asked Questions

1. What is Keploy's Unit Test Generator (UTG)? <br>
    - Keploy's UTG automates the creation of unit tests based on code semantics, enhancing test coverage and reliability.

2. Does Keploy send your private data to any cloud server for test generation?<br>
    - No, Keploy does not send any user code to remote systems, except when using the unit test generation feature. When using the UT gen feature, only the source code and the unit test code will be sent to the Large Language Model (LLM) you are using. By default, Keploy uses - litellm to support vast number of LLM backends. Yes, if your organization has its own LLM(a private one), you can use it with Keploy. This ensures that data is not sent to any external systems.

3. How does Keploy contribute to improving unit test   coverage?<br>
    - By providing a zero code platform for automated testing, Keploy empowers developers to scale up their unit test coverage without extensive coding knowledge. This integration enhances testing reports, ultimately boosting confidence in the product's quality.

4. Is Keploy cost-effective for automated unit testing?<br>
   - Yes, Keploy optimizes costs by automating repetitive testing tasks and improving overall test efficiency.

5. How does Keploy generate coverage reports?<br>
    - Keploy generates detailed Cobertura format reports, offering insights into test effectiveness and code quality.

6. Can Keploy handle large codebases efficiently?<br>
   - Yes, Keploy is designed to handle large codebases efficiently, though processing time may vary based on project size and complexity.

# 🙋🏻‍♀️ Questions? 🙋🏻‍♂️

Reach out to us. We're here to answer!

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)


# 📝 Sample QuickStarts
- ![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white) : Try a unit-gen on [Mux-SQL](https://github.com/keploy/samples-go/tree/main/mux-sql#create-unit-testcase-with-keploy) app

- ![Node](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node&logoColor=white) : Try a unit-gen on [Express-Mongoose](https://github.com/keploy/samples-typescript/tree/main/express-mongoose#create-unit-testcase-with-keploy) app

## 🌐 Language Support

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)

Other language may be supported, we've not tested them yet. If your **coverage reports** are of **Cobertura format** then you should be able to use keploy-gen in any language.

## Dev Support

Keploy-gen is not just a project but an attempt to make developers life easier testing applications.
It aims to simplify the creation and maintenance of tests, ensuring high coverage, and adapts to the complexity of modern software development.

#### Prompt Generation

Referred from [Meta's research](https://arxiv.org/pdf/2402.09171), the four primary prompts used in the deployment for the December 2023 Instagram and Facebook app test-a-thons

| Prompt Name           | Template                                                                                                                                                                                                                                                                                                                                                                                         |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| extend_test           | Here is a Kotlin unit test class: {`existing_test_class`}. Write an extended version of the test class that includes additional tests to cover some extra corner cases.                                                                                                                                                                                                                          |
| extend_coverage       | Here is a Kotlin unit test class and the class that it tests: {`existing_test_class`} {`class_under_test`}. Write an extended version of the test class that includes additional unit tests that will increase the test coverage of the class under test.                                                                                                                                        |
| corner_cases          | Here is a Kotlin unit test class and the class that it tests: {`existing_test_class`} {`class_under_test`}. Write an extended version of the test class that includes additional unit tests that will cover corner cases missed by the original and will increase the test coverage of the class under test.                                                                                     |
| statement_to_complete | Here is a Kotlin class under test {`class_under_test`} This class under test can be tested with this Kotlin unit test class {`existing_test_class`}. Here is an extended version of the unit test class that includes additional unit test cases that will cover methods, edge cases, corner cases, and other features of the class under test that were missed by the original unit test class: |

Limitation: This project currently doesn't generate quality fresh tests if there are no existing tests to learn from.

Enjoy coding with enhanced unit test coverage! 🫰
