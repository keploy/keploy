package com.example.product.models;

import com.fasterxml.jackson.annotation.JsonProperty;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@NoArgsConstructor
@AllArgsConstructor
public class Response {
    @JsonProperty("body")
    private Object body;

    @JsonProperty("headers")
    private Headers headers;

    @JsonProperty("status")
    private int status;
}
