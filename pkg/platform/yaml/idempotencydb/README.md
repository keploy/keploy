# How it works
- I modified the record service by adding idempotencydb to have a understandable well integrated code with the codebase of keploy.

- It starts from record.go file where it intersects the workflow of the upcoming testcase being saved into a yaml file.
- It then checks the testcase to see if it is a request replayed by the idempontency replayer by checking the header, if it is, it will continue ignoring the saving of this replayed request.
- If not, then it is a new upcoming testcase, it will save the testcase into a yaml file. but before that I take this testcase and call ReplayTestCase() function.
- ReplayTestCase() function checks idempotency of the request by replaying it and watching for any inconsistencies in the responses by comparing and removing the non-deterministic data from the response.
- after comparing the responses, it will save them and the comparison result into a yaml file under the test-set directory.

- By doing this, it will only detect the data changes across multiple runs but it will not detect other dynamic headers and other session related data that could lead to flaky tests.
- For the dynamic headers and session related data, I have written a function to save the detected dynamic headers and session related data into a yaml file under the test-set directory.
- Having a config file that enables the user to ignore or updated certain headers and session related data across all tests in the test-set.
- Will give the user warning about these detect headers and ask him to update the config file to ignore or update them on testing, this simple solution will reduce flaky tests by giving the user the option to ignore or update header that maybe leads to inconsistencies.