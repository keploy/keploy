package com.example.order.models;

import com.fasterxml.jackson.annotation.JsonProperty;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@NoArgsConstructor

public class Headers {
     @JsonProperty("Content-Type")
    private String contentType;
        //create full arg constructor
    public Headers(String contentType) {
        this.contentType = contentType;
    }

}
