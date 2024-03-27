package com.example.order.models;

import com.fasterxml.jackson.annotation.JsonProperty;

import lombok.Data;

@Data

public class Request {
    
    @JsonProperty("body")
    private Object body;

    @JsonProperty("headers")
    private Headers headers;
    @JsonProperty("URL")
    private String URL;

    @JsonProperty("method")
    private String method;

    @JsonProperty("path")
    private String path;

    @JsonProperty("query")
    private String query;
    //add full arg constructor
    public Request(Object body, Headers headers, String URL, String method, String path, String query) {
        this.body = body;
        this.headers = headers;
        this.URL = URL;
        this.method = method;
        this.path = path;
        this.query = query;
    }
    //add no arg constructor
    public Request() {
    }

}
