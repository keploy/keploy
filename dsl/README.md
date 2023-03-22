## DSL For Keploy

- HCL is a configuration language created by HashiCorp. It is a simple, structured language that is easy to read, write, and maintain. It is an extensible language, which means that there are no limitations on adding new syntax.

- Using HCL, we can create a DSL for Keploy. Another plus point of using HCL is that it is already has an interpreter written in Go. So, we can use it directly in our code.

- The example `test.hcl` file I have created is as follows:

```
request get "test_get_request"{
    url = "http://httpbin.org/get"
}
```

- The above code will create a get request to the provided url. We can add more code blocks to extend the functionality of the DSL.

```
request post "test_post_request"{
    url = "http://httpbin.org/post"
    headers = {
        Content-Type = "application/json"
    }
    body = {
        "payload" = "{\"firstname\":\"John\"}"
    }
}
```

- The above code will create a post request to the provided url. We can add more code blocks to extend the functionality of the DSL.

- We can add assertion blocks to check the response of the request.

- We can also create functions to extend the functionality of the DSL.

- To parse `test.hcl` file , provide the path of the file to the `Run()` function in `dsl/run.go` file

- The parsing logic is present in the `dsl/lang` directory.
