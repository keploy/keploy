package com.example.product.models;

import java.util.List;

import com.fasterxml.jackson.annotation.JsonProperty;

import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@NoArgsConstructor

public class Contract {
     @JsonProperty("consumer")
    private Participant consumer;

    @JsonProperty("interaction")
    private List<Interaction> interactions;

    @JsonProperty("provider")
    private Participant provider;

}
