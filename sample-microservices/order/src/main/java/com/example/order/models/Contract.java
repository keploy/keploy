package com.example.order.models;

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
    //create full arg constructor
    public Contract(Participant consumer, List<Interaction> interactions, Participant provider) {
        this.consumer = consumer;
        this.interactions = interactions;
        this.provider = provider;
    }

}
