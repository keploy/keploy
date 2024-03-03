package com.example.order.models;

import com.fasterxml.jackson.annotation.JsonProperty;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@NoArgsConstructor
@AllArgsConstructor
public class Interaction {
    @JsonProperty("description")
    private String description;

    @JsonProperty("request")
    private Request request;

    @JsonProperty("response")
    private Response response;
}
