

request get "test_get_request"{
    url = "http://httpbin.org/get"
}

request post "test_post_request"{
    url = "http://httpbin.org/post"
    headers = {
        Content-Type = "application/json"
    }
    body = {
        "payload" = "{\"firstname\":\"John\"}"
    }
}

