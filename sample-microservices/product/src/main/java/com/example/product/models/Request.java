package com.example.product.models;

import com.fasterxml.jackson.annotation.JsonProperty;

import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@NoArgsConstructor
public class Request {
    
    @JsonProperty("body")
    private Object body;

    @JsonProperty("headers")
    private Headers headers;

    @JsonProperty("method")
    private HttpMethod method;
    @JsonProperty("URL")
    private String URL;
    @JsonProperty("path")
    private String path;

    @JsonProperty("query")
    private String query;

}
