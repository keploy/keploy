This test uses Jest as the testing framework and assumes that the GITHUB_TOKEN environment variable is set 
with a valid GitHub personal access token.

The first test checks that the wrappedNodeFetch function returns a promise that resolves to the expected response object 
for a successful API request. It sends a GET request to the GitHub API to fetch the profile information of the 
octocat user and asserts that the response status code is 200, the status text is "OK", the content type is JSON,
 and the response body contains a login property with the value octocat.

The second test checks that the function throws an error for a non-existent or example GitHub user. 
It sends a GET request to the GitHub API with an invalid username and asserts that the function rejects 
the promise with an error.