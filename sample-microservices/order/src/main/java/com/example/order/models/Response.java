package com.example.order.models;

import com.fasterxml.jackson.annotation.JsonProperty;

import lombok.Data;

@Data

public class Response {
    @JsonProperty("body")
    private Object body;

    @JsonProperty("headers")
    private Headers headers;

    @JsonProperty("status")
    private int status;
    //add full arg constructor
    public Response(Object body, Headers headers, int status) {
        this.body = body;
        this.headers = headers;
        this.status = status;
    }
    //add no arg constructor
    public Response() {
    }

}
