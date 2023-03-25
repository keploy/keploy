## How do we take schema from user and load in keploy-

#### - Define the schema format: we can use a standard schema language such as JSON, YAML, or XML Schema, or define our own format.
#### - Create a schema loader: that can load the schema from the user. This loader can be implemented in GO or other programming languages that Keploy supports.
#### - Validate the schema: Once the schema is loaded, we can validate it to ensure that it meets the requirements for our tests.
#### - Save the schema: We can save this schema in new folder 'schema' like 'mocks' and 'tests' and make them visible on Web GUI and CLI as well. 
#### - Use the schema to configure tests: Once the schema is loaded and validated, we can use it to configure our tests in Keploy. This may involve specifying the input and output formats for tests, defining any pre-processing or post-processing steps that are required, and specifying any other testing parameters.

## How to convert that schema and produce automated tests which has almost every possible testcase-

#### - Load the schema from 'schema' folder.
#### - Parse the JSON Schema and extract its properties, constraints, and validation rules.
#### - Generate a set of test cases that cover as many edge cases and scenarios as possible based on the properties and constraints of the schema, we can use the below mentioned methods to generate test cases.
#### - Write test functions that validate the test cases against the JSON Schema and report any errors or failures.
#### - Integrate the test functions into into testing framework as new 'Auto Generated Test Cases' feature instead of adding them into 'Record' feature.

## How do we show the test cases generated to user-

#### We can use a new 'Auto Generated Test Cases' feature instead of adding them into 'Record' feature and a new mode:
```
export KEPLOY_MODE="autogeneratetestcases"
```
#### to auto generate test cases and add this feature to Web GUI and CLI. After running KEPLOY in this mode a new folder can be generated to store the auto generated test cases these generated test cases would be even visible to user in GUI and in 'test' mode even this new test cases would be tested or we can even create one more new mode to test auto generated test cases.

## What techniques we use for generating tests-

#### - Boundary Values Testing: This technique involves testing the API with inputs that are at the boundaries of the allowed range. It can help identify issues with input validation and ensure that the API behaves correctly when handling edge cases.
#### - Random Testing: This approach involves generating random inputs and testing the API with them. It can be useful for exploring unexpected behavior in the API that might not be apparent with a more structured testing approach.
#### - Combinational Testing: This technique involves generating possible combinations of input parameters to test the API. It can be particularly useful when testing APIs with many input parameters, as it helps ensure that all combinations of inputs have been tested.
#### - ML model: Machine learning can be used to create a model for generating test cases. The model can be trained on existing test cases and can then generate new test cases based on the API schema and response. This approach can be particularly useful for generating large numbers of test cases quickly and efficiently.
#### - Swarm testing: This technique involves generating a large number of tests by combining different input values in various ways. The goal is to test the system with a large number of diverse inputs to increase the likelihood of finding bugs.
#### - Fuzz testing: This technique involves generating invalid or unexpected input data to test how the application handles it. It can be effective in finding security vulnerabilities or input validation bugs.
#### - Property-based testing: This technique involves defining properties or invariants that the application should always hold true, and generating tests to check those properties. It can help ensure that the application is functioning correctly under all conditions.
#### These techniques can be used in combination to generate automated tests for an application.

## If we get a json object instead of string how do you deal with that-
We can use the github.com/xeipuuv/gojsonschema library in GO to validate the input JSON and save this JSON object and extract feilds from it to further generate test cases from it  