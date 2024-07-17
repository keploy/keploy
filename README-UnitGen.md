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

# Keploy Configuration Guide 📄🚀

Keploy is a powerful tool for testing and evaluating API responses. This guide provides instructions and examples on how to configure noise parameters, including handling deeply nested JSON fields in the Keploy configuration file.

## Table of Contents
- [Global Noise](#global-noise)
- [Handling Deeply Nested JSON Fields](#handling-deeply-nested-json-fields)
- [Important Notes](#important-notes)

## Global Noise 🌐

The `globalNoise` section is used to define parameters that are globally ignored for all API calls during testing. This helps filter out consistent noise, ensuring a cleaner evaluation of responses.

### Example Configuration 🛠️

```yaml
globalNoise:
  global: 
    body: 
      # Example: Ignore the entire 'token' field in the nested structure
      "data.signUp.token": []
```
### Explanation 📘
Handling deeply nested JSON fields in the Keploy configuration file involves specifying the exact path to the field you want to ignore. For instance, to ignore the `token` field inside the `signUp` object within the `data` object, use the following configuration:

```yaml
globalNoise:
  global: 
    body: 
      "data.signUp.token": []
```
This configuration ensures that the `token` field is ignored during API response evaluations, improving the accuracy of your testing results.

## Handling Deeply Nested JSON Fields 🔄
When dealing with deeply nested JSON structures, it's essential to accurately specify the path to the field you wish to ignore. Here’s how you can configure Keploy to handle such scenarios:

### Example: Ignoring Nested Fields 🛠️
Consider the following JSON response structure:

```json
{
  "data": {
    "signUp": {
      "id": "100",
      "email": "keploy@test.com",
      "firstName": "keploy",
      "lastName": "keploy",
      "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
    }
  }
}
```

To ignore the `token` field in the `signUp` object within the `data` object, use the following configuration:

```yaml
globalNoise:
  global: 
    body: 
      "data.signUp.token": []
```

This configuration effectively filters out the `token` field during API testing, ensuring that it does not interfere with your evaluation process.

## Important Notes 📝
1. Adding fields with a single level of nesting is straightforward in Keploy configuration files.
2. For multiple levels of nesting, specify each level explicitly in the configuration to accurately target the field to be ignored.
3. Use regular expressions or empty arrays as needed to configure noise filtering based on your testing requirements.

By following these examples and guidelines, you can effectively manage nested JSON fields in the Keploy configuration file, enhancing the robustness of your API testing processes.


# 🙋🏻‍♀️ Questions? 🙋🏻‍♂️

Reach out to us. We're here to answer!

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-2dno1yetd-Ec3el~tTwHYIHgGI0jPe7A)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![Twitter](https://img.shields.io/badge/Twitter-%231DA1F2.svg?style=for-the-badge&logo=Twitter&logoColor=white)](https://twitter.com/Keployio)


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
