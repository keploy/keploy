package com.example.product.models;

import com.fasterxml.jackson.annotation.JsonProperty;

import lombok.Data;
import lombok.NoArgsConstructor;


@Data
@NoArgsConstructor
public class Participant {
     @JsonProperty("name")
    private String name;
}
